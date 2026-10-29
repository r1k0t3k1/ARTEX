import {
  Activity,
  Ban,
  BellRing,
  Bot,
  Brain,
  Bug,
  ClipboardList,
  FolderOpen,
  FolderSync,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Plug,
  Radio,
  ScrollText,
  Settings2,
  ShieldAlert,
  Sparkles,
  Target,
  Terminal,
  Wrench,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    label: "機能",
    items: [
      { id: "dashboard", title: "ダッシュボード", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", title: "チャット", url: "/chat", icon: MessageSquare },
      { id: "tasks", title: "タスク", url: "/function/tasks", icon: Target },
      { id: "findings", title: "検出結果", url: "/function/findings", icon: Bug },
      { id: "traffic", title: "トラフィック", url: "/function/traffic", icon: Activity },
      { id: "commands", title: "ツール実行", url: "/function/commands", icon: Terminal },
      { id: "llm-records", title: "LLM 録画", url: "/function/llm-records", icon: Radio },
      { id: "assets", title: "資産", url: "/function/assets", icon: Network },
      { id: "sync", title: "資産同期", url: "/function/sync", icon: FolderSync },
      { id: "workspace", title: "ワークスペース", url: "/function/workspace", icon: FolderOpen },
    ],
  },
  {
    id: 2,
    label: "システム",
    items: [
      { id: "llm", title: "LLM", url: "/system/llm", icon: Brain },
      { id: "agents", title: "Agent", url: "/system/agents", icon: Bot },
      { id: "mcp", title: "MCP", url: "/system/mcp", icon: Plug },
      { id: "skills", title: "Skill", url: "/system/skills", icon: Sparkles },
      { id: "tools", title: "ツール", url: "/system/tools", icon: Wrench },
      { id: "notify", title: "通知配信", url: "/system/notify", icon: BellRing },
      { id: "intercept", title: "インターセプトルール", url: "/system/intercept", icon: ShieldAlert },
      { id: "asset-intercept", title: "資産インターセプト", url: "/system/intercept/assets", icon: Ban },
      { id: "approvals", title: "承認記録", url: "/system/intercept/approvals", icon: ClipboardList },
      { id: "logs", title: "ログ", url: "/system/logs", icon: ScrollText },
      { id: "settings", title: "システム設定", url: "/system/settings", icon: Settings2 },
    ],
  },
];
