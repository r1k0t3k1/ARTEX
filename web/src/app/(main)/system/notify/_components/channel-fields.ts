// チャネルフィールド定義と設定値の解析ユーティリティ。
//
// ページと分離しているのは、これが**データ**でありビューではないため。各チャネルが
// どのフィールドを持ち、それぞれどのコントロールを使うか、フォームテキストと設定値
// （JSON）の双方向変換を記述する。別ファイルにしておけば、チャネル追加時にここだけ
// を変更すればよく、ページ本体は触らなくて済む。
// チャネル種別の表示名と概要。フロントエンドに置くのは文言にしか影響せず、
// バックエンドが知る必要がないため。
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Lark",
  wecom: "WeCom",
  webhook: "汎用 Webhook",
  telegram: "Telegram",
  email: "メール",
};

// 各チャネルの設定フィールド定義。
//
// ここでは意図的にフロントエンド側のフィールド表を保持し、バックエンドから
// schema を配信しない。バックエンドは Validate（必須/形式）だけを担い、UI が
// 必要とするのはレイアウトとコントロール種別であり、関心事が異なるため。
// 唯一の結合点は secret_keys —— どのフィールドをパスワード入力にするかは
// バックエンドが返す。どの値が資格情報かはチャネル実装だけが知っているため
// （WeCom は Webhook 全体が資格情報だが、DingTalk は secret の 1 つだけ）。
// チャネル追加時にここへ項目を足し忘れても、フォームが空になるだけで
// 静かに壊れることはない（下の hasFields が警告する）。
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "署名キー",
      kind: "password",
      help: "ボットのセキュリティ設定で「署名」を選んだ場合に入力します。「カスタムキーワード」またはセキュリティ設定なしの場合は空欄にします",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    {
      key: "secret",
      label: "署名検証キー",
      kind: "password",
      help: "ボットで「署名検証」を有効にした場合に入力します。それ以外は空欄にします",
    },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "送信先 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "HTTP メソッド",
      kind: "select",
      options: [
        { value: "POST", label: "POST（リクエストボディあり）" },
        { value: "PUT", label: "PUT（リクエストボディあり）" },
        { value: "PATCH", label: "PATCH（リクエストボディあり）" },
        { value: "GET", label: "GET（リクエストボディなし）" },
      ],
    },
    {
      key: "headers",
      label: "カスタムリクエストヘッダ",
      kind: "kv",
      help: "1 行ごとに KEY=VALUE。例: Authorization=Bearer xxx",
    },
    {
      key: "body_template",
      label: "リクエストボディのテンプレート",
      kind: "textarea",
      help:
        "空欄の場合は内蔵の既定テンプレートを使用します。変数：{{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}、" +
        "および range .Items 内の .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel。" +
        "文字列を挿入する場合は {{.Xxx}} ではなく {{json .Xxx}} を使用してください。そうしないとタイトル内の引用符が JSON を壊します。",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API URL",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "空欄なら公式 URL。セルフホストの Bot API リバースプロキシを使う場合に入力します",
    },
  ],
  email: [
    { key: "host", label: "SMTP サーバー", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "ポート",
      kind: "number",
      placeholder: "587",
      help: "587 は STARTTLS を使用します。465 の場合は「暗黙的 TLS」を有効にしてください",
    },
    { key: "username", label: "アカウント", kind: "text" },
    { key: "password", label: "パスワード / 認証コード", kind: "password" },
    { key: "from", label: "差出人", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "宛先", kind: "list", help: "複数のアドレスはカンマで区切ります" },
    {
      key: "tls",
      label: "暗黙的 TLS",
      kind: "switch",
      help: "465 ポートでは有効、587 では無効のままにします（自動で STARTTLS されます）",
    },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "指定なし" },
  { value: "low", label: "低以上" },
  { value: "medium", label: "中以上" },
  { value: "high", label: "高以上" },
  { value: "critical", label: "深刻のみ" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV は「1 行ごと KEY=VALUE」のテキストエリアを解析する。
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs はカンマ/空白区切りの id リストを解析する。
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords は行/カンマ区切りのキーワードリストを解析する（脆弱性種別名に空白を含み得るため、行またはカンマで分割する）。
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
