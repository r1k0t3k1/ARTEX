import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "アプリ",
  endpoint: "エンドポイント",
  ip: "IP",
  root_domain: "ルートドメイン",
  service: "サービス",
  subdomain: "サブドメイン",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "Agent 検出",
  anchor: "黒板アンカー",
  api: "資産 API",
  company: "企業関連",
  legacy: "履歴関連",
  manual: "手動追加",
  system: "システム関連",
  task: "タスク初期化",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
