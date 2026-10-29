"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// 会話入力欄の送信/改行キー割り当て。純粋なフロントエンド設定で、localStorage にのみ保存し、DB にも保存せずアカウント同期もしない。
// そのためブラウザを変えると再設定が必要。issue #39 参照 —— 0.3.2 で Ctrl+Enter 送信を Enter 送信に変更したため、
// ここで旧キー割り当てを任意の選択肢として戻している。
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter で送信、Shift+Enter で改行" },
  { value: "ctrl-enter", label: "Ctrl+Enter で送信、Enter で改行" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 同一タブ内の購読者集合。localStorage の storage イベントは「他の」タブでのみ発火するため、
// このページで設定を変更した後は emit で同一ページの入力欄に通知する。そうしないとリロードするまで反映されない。
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 返すのは文字列リテラルで Object.is が値比較を行うため、useSyncExternalStore がループに陥ることはない。
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// サーバーには localStorage がないため、まず既定値を描画し、hydrate 後に getSnapshot で補正する。
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey は1回のキー入力で送信すべきかを判定する。
// isComposing / keyCode 229 は中国語などの IME が変換中であることを示すので必ず通す。そうしないと Enter での確定が誤送信になる。
// enter モードは Shift のみを除外し、0.3.2 の挙動と一字一句同じにしている —— 設定を変えないユーザーの操作感を変えないため。
// ctrl-enter モードは Ctrl と Cmd（macOS）の両方を受け付ける。
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
