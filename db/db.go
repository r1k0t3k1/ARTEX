// Package db は ARTEX の PostgreSQL データソース（旧 graph の単一ファイル SQLite を置き換える）。
// 接続を開き、schema を適用し、組み込み agent と変数カタログを seed する。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

// maxOpenConns はコネクションプールの上限で、詳細は Open 内の説明を参照。PostgreSQL の既定値
// max_connections=100 を大きく下回り、かつアプリ自身の入れ子取得の深さ（起動時の schema
// advisory lock は 1 本の接続を保持したまま seedBuiltins に別の接続を取らせる）より十分大きいので、
// 自己デッドロックにはならない。
const maxOpenConns = 32

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// database/sql は既定で接続数を制限しない。プールに空きが無ければ無条件に新規接続を
	// 作り、PostgreSQL の max_connections（既定 100）まで到達して初めて拒否されるため、
	// ピーク時のクエリは `FATAL: sorry, too many clients already` という**エラー**になる。
	// 上限を設ければ超過分のクエリは空き接続を待ってキューに並ぶ——同じ負荷でも失敗ではなく
	// 遅延になるだけなので、呼び出し側は「読めない」と「存在しない」を区別する必要がなくなる。
	// maxOpenConns は psql / reset-password.sh や併存しうる他インスタンス分の余裕を見て決める。
	// max_connections を下げている場合は、ここも合わせて下げること。
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 行作成時の既定の対話型シェル設定；ON CONFLICT ではユーザーが後から切り替えた値を上書きしない
	runSeconds            *int // 行作成時の 1 回の run の実時間上限（秒）；nil=seed 既定値（1200）、0=無制限
}

type promptVar struct{ name, desc, example, source string }

// intp は v のポインタを返し、builtinAgent の任意フィールド（runSeconds など）に明示的に値を入れるために使う。
func intp(v int) *int { return &v }

// builtinAgents は docs §5(a) に対応する。組み込みツールは DB に入れず、ここでは agent + 変数カタログのみを seed する。
// 注：planner/worker/mainagent/auto の対話型シェルは下の interactive_shell_default_v1
// ブロックで一括して true にする（その後の toggle を尊重する）。ここの interactiveShell は
// 「行作成時から既定でオン」にしたい新しい agent にのみ使う。
var builtinAgents = []builtinAgent{
	{"goals", "ゴール分解", "goals", "ペネトレーションテストのタスク目標を、独立して検証可能な複数のサブゴールに分解する。", []promptVar{
		{"EngagementDescription", "タスク説明（テスト対象／背景）", "example.com のサイトをテスト", "exploration"},
		// Now はグローバル runtime 変数（server.globalPromptVars 参照）なので、各 agent のカタログで
		// 重複定義しない。さもないと withGlobalVars で追加する際にグローバル項目と名前が衝突する。
	}, false, nil},
	{"planner", "計画", "planner", "状況を読み、目標を判定し、未カバーの新しい方向が実際にある場合のみ探索意図を補う（タスクごとに 1 回の計画ループ）。", []promptVar{
		{"Goal", "タスクの総合目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "資産数／種別分布の要約（任意）", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "メイン", "main", "人とマシンのインターフェース：進捗を観察し、人の意図を hint または優先度の高い意図として具体化する。", []promptVar{
		{"Goal", "現在のタスク目標", "example.com の管理者権限を取得する", "exploration"},
		{"AssetSummary", "開始時の状況要約（任意）", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "確認済み脆弱性の要約（任意）", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "実行", "worker", "1 つの意図を引き受けて実行し、発見した事実／脆弱性をナレッジグラフに書き戻してから停止する。", []promptVar{
		{"ProxyAddr", "プロキシアドレスを記録（if の 2 分岐文言を駆動）", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の自己識別子（任意）", "worker-1", "runtime"},
	}, false, nil},
	// Auto：組み込みの「プラットフォーム操作」agent。ペネトレーションのオーケストレーションループには参加せず、
	// 会話ページから駆動され、ツールでプラットフォームを操作する。
	{"auto", "Auto", "assistant", "プラットフォーム操作アシスタント：ツールでタスク（作成／閲覧／一時停止／ヒント付与）と資産を管理し、skill・カスタムツール・MCP の作成／変更もできる。", nil, false, nil},
	// pentest：組み込みの「独立ペネトレーション」agent。会話ページから駆動され、1 人で偵察から締めくくりまで
	// ペネトレーションの全チェーンを走り、自分で計画し、自分で実行し、自分で検証する。対話型シェルは既定でオン。
	{"pentest", "ペネトレーションテスト", "assistant", "独立ペネトレーション agent：偵察→攻撃面の特定→深掘りエクスプロイト→検証→締めくくりまで 1 人で全チェーンを走り、自分で計画し、自分で実行し、自分で対抗的検証を行う。", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' を各 agent カタログからグローバル runtime 変数へ昇格させた後も、旧 DB の goals には 'Now' が
	// 1 件残っておりグローバル項目と名前が衝突する（フロントの変数リストで key が重複）。まとめて削除する。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// 組み込みブラウザ（Playwright）MCP を一度だけ seed する。既定は無効（ユーザーが必要に応じて
	// 自分で有効化する）、プロキシも既定では無し。トラフィックキャプチャのトグルが実行時に
	// 記録プロキシ + CA を注入／除去する（server.Manager.syncBrowserMCPProxy）。
	// 存在しない場合のみ INSERT し、再起動時にユーザーの編集（args/env/enabled/
	// visibility）を上書きしないようにする。
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[組み込み] 政府サイト (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[組み込み] 政府サイト (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[組み込み] 教育サイト (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[組み込み] 教育サイト (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── システム破壊的コマンド (priority 100) ──────────────────────────────
		{
			name:     "[組み込み] 再帰的強制削除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "再帰的強制削除（rm -rf / rm --recursive）の実行を禁止。システムやターゲット環境を恒久的に破壊する恐れがある",
			priority: 100,
		},
		{
			name:     "[組み込み] システム重要ディレクトリの削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "システムの重要パスの削除を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクフォーマット mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "ディスクのフォーマット（mkfs）を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクデバイスへの上書き dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd によるディスクデバイスへの上書きを禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] Fork ボム",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork ボムの実行を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] シャットダウン / 再起動",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "シャットダウンまたは再起動コマンドの実行を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] 全プロセスの強制終了",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 または killall -9（全プロセスの強制終了）を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスク消去 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "ディスクデバイスに対する shred/wipe 消去を禁止",
			priority: 100,
		},
		{
			name:     "[組み込み] ファイアウォールルールの全消去",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "ファイアウォールルールの全消去（iptables -F / nft flush）を禁止",
			priority: 100,
		},
		// ── データベース破壊的操作 (priority 90) ────────────────────────────
		{
			name:     "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 操作の実行を禁止。データベースオブジェクトを不可逆に破棄する恐れがある",
			priority: 90,
		},
		{
			name:     "[組み込み] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE の実行を禁止。データテーブルの全データを消去する恐れがある",
			priority: 90,
		},
		{
			name:     "[組み込み] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB の drop 操作の実行を禁止",
			priority: 90,
		},
		{
			name:     "[組み込み] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis の FLUSHALL / FLUSHDB の実行を禁止。全キャッシュデータを消去する恐れがある",
			priority: 90,
		},
		// ── HTTP 破壊的リクエスト (priority 80) ───────────────────────────────
		// Agent が DELETE リクエストを送る代表的な 3 パターン：
		//   1. curl -X DELETE / --request DELETE（Bash ツールで直接実行、またはスクリプトに記述）
		//   2. Python HTTP クライアントの .delete() メソッド
		//   3. JS／汎用スクリプトでの method: 'DELETE' / method="DELETE"
		{
			name:     "[組み込み] curl / wget による DELETE リクエスト送信",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget による HTTP DELETE リクエストの送信を禁止。ターゲットシステムのデータを削除する恐れがある",
			priority: 80,
		},
		{
			name:     "[組み込み] Python HTTP クライアントの DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP クライアントによる DELETE リクエストの送信を禁止",
			priority: 80,
		},
		{
			name:     "[組み込み] スクリプト内での HTTP DELETE メソッド宣言（JS／汎用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "スクリプト内で HTTP DELETE リクエストを宣言して送信することを禁止",
			priority: 80,
		},
		{
			name:     "[組み込み] 一括消去 / クリア系のエンドポイントパス",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "一括消去や破棄系のエンドポイント（/clear /wipe /flush /purge など）の呼び出しを禁止",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 は、かつて guard.go にハードコードされていた 2 つの安全パターン
// （破壊的シェル + データ外部送信パイプ）を通常のインターセプトルールへ移行する。独自のフラグで
// 制御するので、既に v1 を実行済みの DB にも適用される。旧 guard.go の下限とは異なり、これらは
// 単なる [組み込み] ルールであり、ユーザーが無効化・削除できる。外部送信ルールは既定で無効
// （curl/wget/nc パイプのパターンは正当な CTF／ペネトレーションテストのリバースシェルや
// データ転送パイプを誤検知するため）。外部送信のゲートが実際に必要なときに手動で有効化する。
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[組み込み] 破壊的なシステムコマンド",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破壊的コマンドを拒否（rm -rf / / mkfs / dd / fork bomb / シャットダウン・再起動 / ディスクデバイスへの上書き）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[組み込み] データ外部送信パイプ",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "データ外部送信の疑いがあるパイプを拒否（コマンド出力を curl/wget/nc 経由で外部送信）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[組み込み] 削除系エンドポイントパス"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"削除系エンドポイント（/delete /remove /unlink /erase など）の呼び出しを禁止。HTTP メソッドを問わない——多くのアプリは削除エンドポイントを GET/POST でも発火でき、やはりターゲットのデータを実際に削除する",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
