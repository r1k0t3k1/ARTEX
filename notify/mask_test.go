package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("マスク値が完全な資格情報を漏らした: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("マスク値はアドレス本体を露出すべきではない: %q", got)
	}
	// 末尾 6 桁は保持し、ユーザーがどのボットかを識別できるようにする。
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("識別の手がかりとして末尾 6 桁を保持すべき: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("マスク値は IsMasked で識別できなければならない: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 短い資格情報で末尾 6 桁を露出すると、資格情報全体を露出したのと同じになる。
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("長さ %d の資格情報には末尾のヒントを与えるべきではない、得られたのは %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("マスク値が元の値を含んでいる: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s はマスクすべき、得られたのは %q", k, s)
		}
	}
	// 資格情報でないフィールドはそのまま保持すべき。さもないと UI で表示できない。
	if masked["port"] != float64(587) {
		t.Errorf("資格情報でないフィールド port を変更すべきではない: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// チャネル種別を識別できないときは、UI に空の設定を表示する方が、資格情報を
	// 含みうる生の内容を返すよりまし。
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("未知のチャネル種別は空の設定を返すべき、得られたのは %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// マスクは表示層の振る舞いであり、逆に DB の真の値を書き換えてはならない。
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig が入力を変更した。実際の資格情報がマスク値で上書きされてしまう")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// ユーザーは method だけを変更し、ブラウザが送信したのはマスク値 + 新しい method。
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("マスクされたフィールドは DB の元の値を保持すべき、得られたのは %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("変更されたフィールドは反映すべき、得られたのは %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空文字列はそのフィールドをクリアすべき、得られたのは %v", got)
	}
	// 言及されていないフィールドは保持する（部分更新の意味論）。
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("言及されていないフィールドは保持すべき、得られたのは %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("言及されていないフィールドは保持すべき、得られたのは %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("言及されたフィールドは更新すべき、得られたのは %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap は本パッケージで最も重要なセキュリティ
// 不変条件の 1 つ：**宛先アドレスを変えるとき、古い資格情報を持ち越してはならない**。
//
// ここのケースはまさに攻撃の形の入力（アドレスだけ変え、資格情報には触れない）を
// 使っており、「防御ロジックへの正しい入力」ではない——後者だけをテストすると、
// 防御が効いていなくてもすべてグリーンになってしまう。
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing は名指しされるべき資格情報キー。
		wantMissing string
	}{
		{
			name: "汎用 Webhook でアドレスを変えつつ Authorization ヘッダーを流用",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram で base_url を変えつつ Bot Token を自分のエンドポイントへ送る",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "メールで SMTP ホストを変えつつパスワードを渡す",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "メールで TLS をオフにする場合もパスワードの再提示が必要",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// マスク値 = 「古い資格情報を流用」であり、アドレス変更の文脈では
			// 同様に拒否しなければならない。
			name:        "マスクされた資格情報 + 新しいアドレスを送信",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk で Webhook を変えつつ署名キーを流用",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("アドレスを変えたのに資格情報を再提示していないので、拒否すべき。得られた設定 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("操作可能なヒントを返せるよう専用のエラー型を返すべき、得られたのは %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("欠落している資格情報キー %q を名指しすべき、得られたのは %v", tc.wantMissing, target.Missing)
			}
			// エラーメッセージは操作者に修正方法を示さなければならない。
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("エラーメッセージに %q を含めるべき: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits は逆方向のケース：正常な編集を
// 誤ってブロックしてはならない。さもないとこの防御は「煩わしすぎる」として
// 迂回されたり削除されたりする。
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "名前だけ変更（設定はそのまま回送）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "リクエストメソッドだけ変更し、アドレスも資格情報も変更しない",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "アドレスを変え、**同時に**新しい資格情報を渡す",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "アドレスを変え、資格情報が不要になったと明示",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram で chat_id（宛先ではない）を変更",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "メールで宛先（宛先アドレスではない）を変更",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("正当な編集が誤ってブロックされた: %v", err)
			}
			if merged == nil {
				t.Fatal("マージ結果を返すべき")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance は誤判定しやすい細部をカバーする：
// フロントエンドが送信するポートは JSON number（float64）で、DB から読み戻す
// ときも float64 だが、2 つの値の型が異なることがある（int と float64 など）。
// == で比較すると「変更なし」を「変更あり」と判定し、名前だけ変えたユーザーに
// 「パスワードを再入力してください」と表示してしまう——誤警告はこの防御への
// 信頼を失わせる。
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 同じポートを int 形式で送信する。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("ポート値が同じ（型だけ異なる）場合はアドレス変更と判定すべきではない: %v", err)
	}
	// 本当にポートを変えた場合は必ずブロックする。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("ポート変更はブロックすべき")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination は「空欄を許す
// 宛先フィールド」の経路をカバーする：Telegram の base_url が空欄なら公式 API
// アドレスを使うという意味。
//
// かつてここで、チャネルが 2 回目の保存以降ずっと保存に失敗していた：
//
//	新規作成時に DB へ base_url:"" を保存（作成経路はフロントエンドが送った config を
//	そのまま保存し、MergeConfig を通さない）
//	→ 1 回目の保存で MergeConfig が空文字列を明示的なクリアとみなし、そのキーを delete
//	→ 2 回目の保存で incoming は依然 ""、しかも stored にはもうキーがないため「アドレスが変わった」と判定
//	→ bot_token はマスクされたエコー値 → 400「宛先アドレスが変更されました。あわせて資格情報フィールドを再入力してください」
//
// ユーザーは何も変えていないのに、Bot Token を貼り直すまで二度と保存できなくなる。
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// フロントエンドの buildConfig() はそのチャネルの各フィールド定義に対して値を
	// 1 つずつ送信する：資格情報はマスクを回填し、空のテキストボックスは空文字列を
	// 送信する。ここでは「変更したキー」だけを送るのではなく、その出力を完全に
	// 再現する。
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 1 回目の保存：チャネル名だけ変更し、config はそのまま回送。
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("1 回目の保存が誤ってブロックされた: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提が変わった：空文字列は MergeConfig に削除されるべき——本ケースがカバーするのはまさに「キーが消えた後」の段階")
	}

	// 2 回目の保存：送信内容は前回と完全に同じで、ユーザーは何も変えていない。
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("2 回目の保存が誤ってブロックされた（ユーザーは何も変えていない）: %v", err)
	}
	// 3 回目で、「1 回だけ失敗する」のではなく安定して保存できることを確認する。
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("3 回目の保存が誤ってブロックされた: %v", err)
	}
	// 資格情報は最後まで保持され、空文字列のロジックに巻き込まれて消えてはならない。
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token は元の値を流用すべき、得られたのは %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges は前のケースと対になる
// 断言：空文字列と「キーが存在しない」を等価とみなしても、本当のアドレス変更まで
// **見逃してはならない**。この両方向とも実際に資格情報が外へ出る経路——Telegram の
// Bot Token は URL のパスに載るので、base_url を変えることは新しいアドレスへ
// Token を渡すことと同じ。
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 方向 1：「空」（公式アドレス）から自前のアドレスへ変更。
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("公式アドレスから自前のアドレスへ変える場合は Token の再入力を要求しなければならない")
	}

	// 方向 2：自前のアドレスを空にする（= 公式 API に戻す）のも同様にアドレス変更。
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("自前のアドレスを空にする（公式 API へ戻す）のもアドレス変更であり、Token の再入力を要求しなければならない")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys と同様：チャネルが宛先キーの宣言を忘れると、PrepareConfigUpdate は
	// それを保護できない。
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("チャネル %s が宛先キーを宣言していない。アドレス変更時の資格情報漏洩対策が効かない", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が資格情報キーを宣言していない", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// コンパイラーは各チャネルに SecretKeys の実装を強制しているが、ここで
	// 「マスクについて白紙回答のチャネルがない」ことを再確認する——空のスライスを
	// 返すチャネルは、その資格情報がブラウザへ平文でエコーされることを意味する。
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("チャネル %s のマスクの期待値がテストに登録されていない", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が資格情報フィールドを 1 つも宣言しておらず、設定が平文でエコーされる", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer は監査で指摘された穴をカバーする：
// マスクの番兵値を**文字列以外**の構造（webhook.headers がオブジェクトなど）に
// 埋め込むと、MergeConfig は「文字列かつ接頭辞付き」だけをマスクと認識するため、
// リテラル "__masked__" が本物のヘッダー値として DB に保存される——以降の認証は
// 静かに失敗し、エラーも一切出ない。
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// オブジェクトの内部にマスクの番兵値を紛れ込ませる。
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("構造体の内部にマスクの番兵値が紛れ込んだ場合は拒否すべき（さもないとリテラルが DB に保存される）")
	}
	// オブジェクト全体を送信する場合（本物の新しい値）は通常どおり受け入れる。
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("新しいリクエストヘッダーの正常な送信はブロックすべきではない: %v", err)
	}
}
