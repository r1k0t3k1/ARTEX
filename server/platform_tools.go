package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// プラットフォーム操作ツール(組み込み Auto agent 用)：skill・カスタムツール・MCP の作成/変更。
// いずれも host ツールで、tools テーブルへ seed して既定で auto にバインドし、hostTools 経由で
// 注入する。既存の db/ファイルシステムのロジックを再利用する。

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys are the tool keys the Auto agent gets bound by default.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- assets ----

// toolDeleteAssetsByHost は 1 つの host に紐づくすべての資産を完全一致で物理削除する。
// プラットフォーム層のツール(per-task ツールではない)：グローバルでタスク横断の資産庫を操作する。
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"host を指定して資産を完全一致で削除する：その host のドメイン/サブドメイン、および配下のサービス(service)・エンドポイント(endpoint)を削除する。\n"+
			"host は完全一致(小文字化・空白除去)。あいまい一致やワイルドカードではない。\n"+
			"ルートドメイン(例：example.com)を渡すとそのサブドメインとサービス/エンドポイントも連鎖削除する。サブドメイン(例：a.example.com)や IP を渡すと、その host 自身とサービス/エンドポイントのみ削除する。\n"+
			"⚠️ 物理削除であり、グローバルな資産庫(タスク間で共有)に作用し、元に戻せない。",
		objSchema(map[string]any{
			"host": strParam("削除する host：ドメイン/サブドメイン/IP。完全一致。例：example.com、a.example.com、1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("資産庫が初期化されていません"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host を空にはできません"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("削除に失敗: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- skills ----

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"新しい skill を作成する(SKILL.md を書き込む。agentskills.io 仕様)。name は小文字英字/数字/ハイフン。",
		objSchema(map[string]any{
			"name":         strParam("skill 名(小文字英字で始まり、英字/数字/ハイフン)"),
			"description":  strParam("skill の説明(必須。何をするか/いつ使うかを書く)"),
			"instructions": strParam("Markdown 本文の説明(任意)"),
		}, "name", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill 名が不正です(小文字英字で始まり、英字/数字/ハイフンのみ、≤64)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description は必須です"), nil
			}
			path := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(path); err == nil {
				// フロントエンド(web/src/app/(main)/system/skills/page.tsx)が
				// msg.includes("已存在") で判定しているため、互換性のため「已存在」の
				// 部分だけは中国語のまま残す。
				return actool.Errorf("skill 已存在: " + a.Name), nil
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var b strings.Builder
			b.WriteString("---\n")
			fmt.Fprintf(&b, "name: %s\n", a.Name)
			fmt.Fprintf(&b, "description: %s\n", a.Description)
			b.WriteString("---\n")
			if strings.TrimSpace(a.Instructions) != "" {
				b.WriteString(a.Instructions)
			} else {
				fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", a.Name)
			}
			if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
				_ = os.RemoveAll(path)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"ある skill 内の 1 ファイル(既定は SKILL.md)を書き込む/上書きする。スキル内容の変更やスクリプト/参照の追加に使う。",
		objSchema(map[string]any{
			"name":    strParam("skill 名"),
			"file":    strParam("相対パス(任意。既定は SKILL.md。例：scripts/run.py)"),
			"content": strParam("ファイルの完全な内容"),
		}, "name", "content"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill 名が不正です"), nil
			}
			skillPath := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(skillPath); os.IsNotExist(err) {
				return actool.Errorf("skill が存在しません: " + a.Name), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("不正なパス: " + msg), nil
			}
			full := filepath.Join(skillPath, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- custom tools ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("モデルへ渡す説明"),
		"kind":        strParam("shell | command | script(Python のみ) | http。shell= bash 環境の宣言(このツールが bash 内で直接呼べることをモデルに伝えるだけで、exec/schema は不要)。残り 3 種類は exec が必要"),
		"exec":        map[string]any{"type": "object", "description": "実行仕様(shell 種別では不要)：command→{command}; script→{code}; http→{method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "引数 JSON-Schema(shell/command/script は空でよい。http は必須で properties を含むこと)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "バインドする agent key(任意)"},
		"deferred":    map[string]any{"type": "boolean", "description": "遅延にするか(shell 種別では無効。command/script/http のうち使用頻度の低いツールのみ有効化する)"},
		"enabled":     map[string]any{"type": "boolean", "description": "有効にするか(既定 true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "【重要】プラットフォームにないツールをインストールした場合は、このツールを呼び出してインストール済みツールをプラットフォームに登録し、プラットフォームから呼べるようにすること！カスタムツール(shell/command/script/http)を作成する。shell= bash 環境の宣言で、key+description+agents だけでよく、exec/schema は不要。",
		customToolSchema("ツール key(小文字英字で始まり、英字/数字/アンダースコア)"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key は小文字英字で始まり、小文字英字/数字/アンダースコアのみ使用できます"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind は shell / command / script / http のいずれかである必要があります"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http ツールは引数 JSON Schema の提供が必須です(空にはできません)"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("この key はすでに存在します: " + a.Key), nil
			}
			if err := s.m.pg.CreateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "既存のカスタムツールを変更する(key 指定)。",
		customToolSchema("変更するカスタムツールの key"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("変更できるのはカスタムツールのみです: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind は shell / command / script / http のいずれかである必要があります"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http ツールは引数 JSON Schema の提供が必須です(空にはできません)"), nil
			}
			if err := s.m.pg.UpdateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP サーバー名"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("stdio の起動コマンド(例：npx)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "コマンド引数の配列"},
		"env":       map[string]any{"type": "object", "description": "環境変数 {KEY:VALUE}"},
		"url":       strParam("http/sse の URL"),
		"enabled":   map[string]any{"type": "boolean", "description": "有効にするか(既定 true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http: TLS 証明書の検証をスキップする(自己署名証明書のときに true、既定 false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "変更する MCP サーバーの id"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "MCP サーバー(stdio/http/sse)を作成する。作成後、そのツールは agent 単位の可視性で認可する必要がある。",
		mcpSchema(false),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name / transport は必須です"), nil
			}
			id, err := s.m.pg.SaveMCP(a.toDB())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "既存の MCP サーバーを変更する(id 指定)。",
		mcpSchema(true),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id は必須です"), nil
			}
			if _, err := s.m.pg.SaveMCP(a.toDB()); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
