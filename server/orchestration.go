package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 本ファイルは P2「クロスタスク・オーケストレーションツール集」(docs/跑分编排 §2 P2) を実装する。
// これらは host ツールであり、Manager(任意タスクの Store)、Engine(一時停止)、タスク作成フローへ
// アクセスする必要があるため server 層に置く。読み取り系ツールは「既存の per-task ツール」を
// 対象タスクの store へリダイレクトして実行する(一時的な ToolSet を作り、対応するツールを
// Call する)ことで、まったく同じロジックを再利用する。制御系(spawn/pause)は Manager/Engine を
// 直接呼ぶ。これらはトラフィックツールと同様に tools テーブルへ seed され、エージェント単位で
// バインドされる(オーケストレーション用エージェントにのみバインドすれば見える)。

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // プラットフォーム操作ツール(skill/ツール/MCP の作成・変更、Auto 用)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 読み込みに失敗: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id は必須です"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("タスクが存在しません: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 汎用の起床通知（専用コールバックのない書き込み操作はこれを通る。読み取りツールでは no-op）
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 「人が N 件の戦略ヒントを追加：…」というトリガーを記録し、プランナーを起床させる
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"すべてのタスクを一覧する(id/説明/ゴール/ステータス/実行時間/親タスク/LLM 設定)。オーケストレーション用エージェントが全体を把握し、どのタスクが長く止まっているか、それぞれどの LLM を使っているかを確認するために使う。実行時間：実行中=作成→現在、終了状態=作成→最終アクティビティ(秒)。llm_profile：タスクのプランナー/ワーカーが使う設定名。(アクティブ設定)=グローバルのアクティブ設定に従う。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(アクティブ設定)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(削除済み)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"利用可能な LLM 設定(profile)を一覧する：id、名前、モデル、形式、現在のアクティブ設定かどうか。id を spawn_task の llm_profile_id 引数に指定すると、サブタスク専用の LLM を指定できる(偵察には安価なモデル、エクスプロイトには強力なモデルなど)。API Key は含まない。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"サブタスクを新規作成して探索エンジンを起動し、task_id を返す。1 つの作業(例：1 問/1 つのゴール)を独立したタスクとして割り当てるのに使う。parent_ref は任意：現在のオーケストレーションに関連する親タスク id を入れると親子関連を付ける。",
		objSchema(map[string]any{
			"description":            strParam("タスク説明(簡潔なタイトル)"),
			"goal":                   strParam("タスクのゴール(何を達成するか)"),
			"parent_ref":             strParam("任意：親タスク id(親子関連を付ける)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("任意：読み取り専用で継承するソースタスク id のリスト(最大 %d 件)。サブタスクはこれらのタスクで判明済みの資産/結論を読み取り専用で参照して起点にできる。parent_ref の純粋な親子ポインタと異なり、これは内容の継承である。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "任意：このサブタスクのプランナー/ワーカーが使う LLM 設定 id(list_llm_profiles 参照)。空なら親タスクを継承し、さらにグローバルのアクティブ設定へフォールバックする"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "任意：タスク単位のタイムアウト(秒)。到達すると優雅な締めくくりを経て timeout 終了状態に入る。空または 0 = 制限なし"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "任意：プランナー心拍のトリガー間隔(秒)。前回の計画終了/タスク開始からこの値に達し、その間にトリガーがない → 計画を 1 ラウンド実行(デッドロックの保険 + 飛行中のワーカーを監督するための起床)。空または 0 = 既定 600(10min)。"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "任意：簡単なタスクで有効にすると、作成時にシード意図(内容=説明+ゴール)を直接発行し、ワーカーが初回プランナーを待たずにテストを開始できる。既定 false(標準どおり先に計画してから実行)。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "名称未設定のタスク"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal は必須です"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 読み取り専用で継承するソースタスク：件数上限 + 各 id の有効性/重複/存在を検証する。
			// 検証ルールは HTTP のタスク作成と同一。
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("関連タスクは最大 %d 件まで選択できます", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("関連タスク id が無効または重複しています"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("関連タスク #%d は存在しません", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 設定 #%d は存在しないか、API Key が未設定です", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 共有の作成後フロー。HTTP のタスク作成(server.go createTask)と同じ launchTask を再利用する：
			// seed + バックグラウンドで可視的にゴール分解(第 0 ラウンド/LLM ステップ/ゴールごと) + engine.Run。
			// seed_first_intent は既定 false(標準どおり先に計画してから実行)。簡単なタスクでは有効にして
			// work を 1 件直接発行しテストできる。
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "指定タスクを一時停止する(そのプランナー/ワーカーのループを停止する)。",
		objSchema(map[string]any{"task_id": strParam("一時停止するタスク id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("タスクが存在しません: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "指定タスクの探索グラフ概要を読む(graph_overview と同じ：資産数/frontier/検出/カバレッジなど)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "指定タスクの確定した脆弱性を読む(flag/PoC を含む。各件に id/task_id/intent_id/vulnclass/severity/サマリー/ステータスを付す)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "指定タスクへ戦略ヒントを注入する(そのタスクのプランナーが次ラウンドで意図を生成するときに読む)。\n"+
		"★バッチを優先：複数のヒントを hints 配列に入れて一度に送信する(ids 配列を返し、hints と同じ長さ・同じ順序。失敗した項目の id=0)。単発なら hints を省略し、トップレベルの text を直接渡す。",
		objSchema(map[string]any{
			"task_id":      strParam("タスク id"),
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】ヒント配列。各要素のフィールドはトップレベルと同じ(text/asset_ids/traffic_refs)。", "items": objSchema(map[string]any{"text": strParam("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[単発] ヒントの内容"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連付ける資産 id(任意。0/1/複数。そのタスク内の資産 id)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"指定タスク内の特定の work(意図)の実行過程を見る：get_task_worker_trace(task_id, intent_id) でステップのサマリーを見る。さらに step_ids=[...] を付けるとそのステップの完全な内容を取得する(1 回最大 5 件。それ以上渡すと先頭 5 件のみ返す)。",
		objSchema(map[string]any{
			"task_id":   strParam("タスク id"),
			"intent_id": map[string]any{"type": "integer", "description": "意図 id(そのタスク内の work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "任意：完全な内容を取得するステップ id(1 回最大 5 件。それ以上渡すと先頭 5 件のみ返し、残りは omitted_step_ids に列挙される)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "指定タスクで実行された work(意図)と各ステップ数を一覧する。どの work を詳しく見る価値があるかの発見に使う(その後 get_task_worker_trace を使う)。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "指定タスク内でキーワードによりすべての work の実行過程を検索する(ヒットしたステップのサマリー + intent_id を返す)。",
		objSchema(map[string]any{"task_id": strParam("タスク id"), "q": strParam("検索キーワード")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"指定タスク内の探索グラフノードの完全な内容を読む(検出/事実/意図/ゴール：サマリー + 詳細/証拠/PoC)。id は探索ノードの id(report_finding が返したもの、または list_task_findings の id)。脆弱性レポートを書く前に、このツールでその脆弱性の完全な証拠を取得する。",
		objSchema(map[string]any{
			"task_id": strParam("タスク id"),
			"id":      map[string]any{"type": "integer", "description": "探索グラフのノード id(資産 id ではない)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"登録済みの脆弱性へ【詳細レポート】を書き込む/更新する(Markdown 全文。段落ごと旧内容を上書きする)。finding_id には report_finding が返した id(\"finding recorded: <id>\" の数字)を渡す。レポートには脆弱性の概要、影響と被害、再現手順、証拠/PoC、修正提案を含めることを推奨する。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "対象の脆弱性 id(report_finding が返した id)"},
			"report":           strParam("詳細レポート全文。Markdown 形式"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic が返した証拠の version。新たな証拠変更でレポートが上書きされるのを防ぐために使う"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「数値または数値文字列」の解析を再利用
			if nodeID <= 0 {
				return actool.Errorf("finding_id が無効です"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d に対応する脆弱性レコードが見つかりません(先に report_finding で登録してください)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform ツールは組み込みの Auto エージェントに既定バインドされる(元々
	// プラットフォーム操作のために作られた存在)。SeedTool は初回挿入のみ有効。既存 DB の
	// seed 済み行は seedAutoDefaultBindings が後から補完バインドする。
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces を worker の既定から解除(一度だけ)
	s.seedWorkerReadbackRebind()  // 旧マイグレーションの誤削除を修正：search_all_worker_traces/get_worker_trace/node_detail を worker へ再バインド(一度だけ)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals プロンプトに「操作制約の抽出手順」を追加 → 既存 DB に新既定を 1 バージョン追加(一度だけ)
	s.reseedMainAgentPrompt()         // mainagent プロンプトに「ゴール達成後に add_intent で意図を直接投入する際、正式なゴールとして登録するか人に問い返す」(一度だけ)
	s.reseedPlannerPrompt()           // planner プロンプト：「0 意図」の正当理由を書き直し + 定量的な受け入れ確認を追加(一度だけ)
	s.reseedWorkerPrompt()            // worker プロンプト：否定的結論の証拠しきい値を追加(一度だけ)
	s.seedReporterAgent()             // 「レポート作成」agent + ツールバインド + finding トリガーをプリセット(一度だけ)
	s.upgradeReporterTriggerMessage() // 既存 DB の補完マイグレーション：reporter に evidence_version を返させる(一度だけ)
	s.seedFindingTrafficTools()       // 任意の証拠引数と読み取り専用の証拠ツールを追加し、ユーザー設定は保持する
	s.seedFindingWorkflowTools()
	// 注：pentest の既定ツールバインドはマイグレーション不要——BuiltinToolSeeds が新規初期化時に
	// list_assets/insert_assets/report_finding/list_findings/list_companies を
	// pentest と一緒に seed 済みである(プロジェクトに旧 DB はまだないため移行しない)。
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool は初回挿入のみなので、新しい引数(例：spawn_task の llm_profile)は
// そうしないと古い DB に届かない。各ツールのエージェントバインドと enabled フラグは保持する。
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 同時に一部の組み込みエージェントツールをコード既定へ刷新する：
	//   - goal_met：旧 DB の seed 済み説明には「今回の計画を終了する」という誤解を招く文言があり、
	//     planner がこれを「空ラウンドを終える」手段と捉え、走り始めたばかりでタスク全体の完了を
	//     誤判定してしまう。
	//   - insert_assets：related 引数(資産が現在のタスクに関連するか、カバレッジに入れるかを示す)
	//     を追加。SeedTool は初回挿入のみなので、旧 DB の seed 済み schema にはこの新引数が届かない。
	//   - list_facts：ページング化し、limit/before/q 引数を追加。旧 DB の seed 済み空 schema では
	//     ツール管理画面に「パラメータなし」と表示され、モデルもこれらの引数説明を得られない。
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform ツールの schema をコード既定へ刷新した(一度だけ)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met の planner 解除に失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt は goals ゴール分解器のプロンプトを【現在のコード既定】へ刷新する——既定本文に
// 「先に操作制約を抽出し(set_constraints)、その後でゴールを分解する」ステップが追加されたが、
// SeedPromptIfEmpty は初回挿入のみなので、旧 DB の既存 version 1 にはこのステップが届かない。
// ここではバージョン管理で【新しいバージョンを追加】して切り替える(ResetPromptToDefault)。
// 旧バージョンは履歴に残り、ユーザーがカスタマイズしていた場合はバージョン記録から戻せる。
// settings フラグでガード → 一度だけ実行。以後既定が変わったらこのフラグを bump する。
// 新規 DB では不要(SeedPromptIfEmpty が最新既定を seed 済み)。
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行する
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 新規 DB でまだ agent 行がない場合、seedPrompts が最新既定を直接 seed するのでこの移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 新規 DB は seedPrompts が最新既定を seed 済み → 現在のバージョンがすでにコード既定と等しいので、
	// 重複バージョンを追加する必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals プロンプトの新既定への再適用に失敗: %v", err)
		return
	}
	log.Printf("[prompts] goals プロンプトに新既定バージョンを追加した(操作制約抽出手順を追加、一度だけ)")
}

// reseedMainAgentPrompt は mainagent プロンプトを【現在のコード既定】へ刷新する——既定本文に
// 「ゴールがすべて達成した後に add_intent で意図を直接投入する際、正式なゴールとして登録するかを
// 人に問い返す」という誘導が追加されたが、SeedPromptIfEmpty は初回挿入のみなので旧 DB の既存
// バージョンには届かない。バージョン管理で【新しいバージョンを追加】して切り替える
// (ResetPromptToDefault)。旧バージョンは履歴に残り、ユーザーがカスタマイズしていた場合は
// バージョン記録から戻せる。settings フラグでガード → 一度だけ実行。新規 DB では不要
// (SeedPromptIfEmpty が最新既定を seed 済み)。reseedGoalsPrompt と完全に同型。
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行する
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 新規 DB でまだ agent 行がない場合、seedPrompts が最新既定を直接 seed するのでこの移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 新規 DB は seedPrompts が最新既定を seed 済み → 現在のバージョンがすでにコード既定と等しいので、
	// 重複バージョンを追加する必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent プロンプトの新既定への再適用に失敗: %v", err)
		return
	}
	log.Printf("[prompts] mainagent プロンプトに新既定バージョンを追加した(ゴール達成後の登録問い返しを追加、一度だけ)")
}

// reseedPlannerPrompt は planner プロンプトを【現在のコード既定】へ刷新する——既定本文を簡潔化・
// 再構成し、「抑制」を重複排除のみに格下げ、「深さ優先をカバレッジより優先」「ハードボトムライン：
// ゴール未達成かつ実行中の意図がなければ必ず何かを産出する」を追加し、否定的結論の再確認に上限を
// 設けた。既定に実質的な変更があるたびに下のフラグを bump する(現在 v2)ことで、既存の旧 DB にも
// 再適用させる。SeedPromptIfEmpty は初回挿入のみで、旧 DB の既存バージョンには届かないため、
// バージョン管理で【新しいバージョンを追加】して切り替える(ResetPromptToDefault)。旧バージョンは
// 履歴に残り、ユーザーがカスタマイズしていた場合はバージョン記録から戻せる。settings フラグで
// ガード → 一度だけ実行。新規 DB では不要(SeedPromptIfEmpty が最新既定を seed 済み)。
// reseedGoalsPrompt と完全に同型。
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行する
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 新規 DB でまだ agent 行がない場合、seedPrompts が最新既定を直接 seed するのでこの移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 新規 DB は seedPrompts が最新既定を seed 済み → 現在のバージョンがすでにコード既定と等しいので、
	// 重複バージョンを追加する必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner プロンプトの新既定への再適用に失敗: %v", err)
		return
	}
	log.Printf("[prompts] planner プロンプトに新既定バージョンを追加した(簡潔化・抑制の格下げ・深さ優先・否定再確認の上限、一度だけ)")
}

// reseedWorkerPrompt は worker プロンプトを【現在のコード既定】へ刷新する——既定本文の record_fact 節から
// 「否定的な結論は観察+試行的な読み方で書く」という一文全体を削除し、confidence(observed/inferred)と
// 「この意図の手段を尽くしたか」を切り離した(これらはプランナーを誤導しやすい)。同時に facts 配列の
// 分割を「互いに完全に独立し、統合できない」ごく少数の例外に絞った。フラグを v3 に bump して既存の
// 旧 DB にも再適用させる。SeedPromptIfEmpty は初回挿入のみで旧 DB の既存バージョンには届かないため、
// バージョン管理で【新しいバージョンを追加】して切り替える。旧バージョンは履歴に残り取り戻せる。
// settings フラグでガード → 一度だけ実行。新規 DB では不要。reseedGoalsPrompt と完全に同型。
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行する
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 新規 DB でまだ agent 行がない場合、seedPrompts が最新既定を直接 seed するのでこの移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 新規 DB は seedPrompts が最新既定を seed 済み → 現在のバージョンがすでにコード既定と等しいので、
	// 重複バージョンを追加する必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker プロンプトの新既定への再適用に失敗: %v", err)
		return
	}
	log.Printf("[prompts] worker プロンプトに新既定バージョンを追加した(コンテキスト確認の節を list_assets/list_findings に収束し、list_facts/node_detail/asset_neighbors を削除、一度だけ)")
}

// reporterToolCallMessage は、レポートを書く前に必ず一度 get_finding_traffic を読むことを
// 無条件に要求する。このツールは読み取り専用で「キャプチャスイッチに依存しない」ため、自動バインドの
// オン/オフにかかわらず手動バインド済みの証拠を読める。ここを「自動バインド有効時のみ読む」と
// 書いてしまうと、既定のオフ設定では reporter が evidence_version を渡さず、
// SetFindingReportVersionByNodeID が legacy セマンティクスで -1 を書き、脆弱性詳細と Markdown
// エクスポートに「証拠が変更されました、レポートの更新待ち」が常駐し、UI 上にはそれを消せる
// 入口が一切なくなる。
const reporterToolCallMessage = "先ほど report_finding によって脆弱性が 1 件登録されました。返却 JSON の finding_id（独立した脆弱性レコード ID）と finding_node_id（探索ノード ID）を読み取り、" +
	"まず get_finding_traffic(finding_id) で現在の証拠一覧とその version を読み取ってください（空の一覧は正常です。通常どおりレポートを書いてください）。" +
	"実行ガイドで自動バインドが有効な場合は、読み取る前に今回の脆弱性のトラフィックを確認して関連付けてください。ノード詳細には finding_node_id を使います。" +
	"最後に update_finding_report(finding_id=finding_node_id, report, evidence_version=実際に読み取ったバージョン) を呼んで保存してください。" +
	"evidence_version は必ず渡してください。渡さないとレポートは永久に更新待ちとマークされます。2 種類の番号を混用しないでください。"

// 旧版のトリガーメッセージ(0.3.8 以前)。これと逐字一致するレコードだけが移行で上書きされ、
// ユーザーが変更したものはそのまま残る。
// この定数は旧 DB の triggers.tool_call_message に保存済みの中国語原文との逐字比較に使うため、
// 互換性のため中国語のまま残す（日本語化すると移行が一致せず、旧 DB に旧文言が残り続ける）。
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage は、旧 DB に残る既定文言のままの reporter トリガーメッセージを
// 新バージョンへ刷新する。seedReporterAgent は reporter_agent_seed_v1 でガードされ、新規 agent
// 作成時にしかトリガーを書かないため、アップグレードした DB には新しい文言が届かない——ツール
// schema は seedFindingTrafficTools が evidence_version を補ったが、reporter にそれを使うよう
// 伝えるものが何もない。一度だけ実行し、未変更の文言だけを上書きする。
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 一度だけ試行する
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] トリガーの読み取りに失敗: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // ユーザーが変更した、または finding トリガーでない場合は何もしない。
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] トリガーメッセージの更新に失敗: %v", err)
			return
		}
		log.Printf("[reporter] トリガーメッセージを evidence_version を読み取って返す形式へ更新した")
	}
}

// seedReporterAgent は「レポート作成」カスタム agent(builtin=false、UI で編集/削除可能)を
// プリセットする：update_finding_report + タスク照会ツールをバインドし、「report_finding が
// 呼ばれたら即トリガー」のトリガーを付ける——脆弱性が登録されるたびに起動して詳細レポートを
// 書かせる。一度だけ実行(settings フラグでガード)：ユーザーが削除した後は再作成しない。
// 依存：orchestration ツールは本関数より上で SeedTool により DB 登録済みなのでバインドできる。
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行する

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key がすでに使用済み(ユーザーが手動作成した)——上書きしない
	}
	a, err := s.m.pg.CreateAgent("reporter", "レポート作成",
		"脆弱性詳細レポートの作成：脆弱性の発見時に自動でトリガーされ、証拠と実行過程を取得して Markdown レポートを書いて書き戻す。")
	if err != nil {
		log.Printf("[reporter] agent の作成に失敗: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] prompt の seed に失敗: %v", err)
	}
	// トリガー実行ポリシー：parallel + none —— 1 脆弱性 1 レポート、複数の finding は並列に
	// それぞれ書く。merge は必ず none にする。そうしないと(既定の all)一度に来た finding が
	// 1 回の実行にまとめられ、並列の意味がなくなる。maxParallel=5：同時に最大 5 つのレポート
	// セッションとし、瞬間的に LLM 呼び出しが増えすぎないようにする。
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] トリガー実行ポリシーの設定に失敗: %v", err)
	}
	// 必要なツールをバインドする：レポート作成 + 証拠/実行過程/状況の読み取り。
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] ツールのバインドに失敗: %v", err)
	}
	// トリガー：report_finding が呼ばれたら即トリガー(ツールは "finding recorded: <id>" を返し、
	// その finding_id とタスク id がトリガーメッセージに入る)。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] トリガーの作成に失敗: %v", err)
	}
	log.Printf("[reporter] 「レポート作成」agent + finding トリガーをプリセットした")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding の既定バインドに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding の既定バインドに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets の既定バインドに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner の既定バインド切り替え
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope の既定バインドに失敗: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope の解除に失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s の worker からの解除に失敗: %v", k, err)
			return // エラー時はフラグを立てず、次回起動時に再試行する
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 振り返り/詳細ツールの再バインドに失敗: %v", err)
		return // エラー時はフラグを立てず、次回起動時に再試行する
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 旧資産ツール名を置換し、insert_assets/add_company_scope を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 資産ツール：Auto はプラットフォーム操作で資産の閲覧/登録や企業範囲の管理をよく行う。
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 既定バインドに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
