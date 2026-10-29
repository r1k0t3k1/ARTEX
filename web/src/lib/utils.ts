import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// ドロワー/ダイアログ（Sheet/Dialog）の onInteractOutside による閉じ判定の補助。
//
// 背景: ドロワー内の Radix ポップアップ（Select ドロップダウン、DropdownMenu、Popover など）はドロワーの
// 外へポータルされる。ポップアップを開いたままオーバーレイ/ドロワー外をクリックして閉じようとすると、この pointerdown は Select と Sheet の
// 2つの DismissableLayer に同時に処理される。Select が先に閉じ、しかも discrete イベントで React が同期的に flush するため、
// Sheet のハンドラに回る頃にはポップアップの data-state は既に closed になっている —— 「その瞬間」にポップアップが開いているかを検出するのは
// 本質的に信頼できない（実測で確認済み）。
//
// 正しい方法: Radix の pointerdown 監視はバブリング段階にある。そこで capture 段階（それより前）で
// 「この瞬間にポップアップが開いているか」を先に記録し、onInteractOutside でその記録値を見て閉じを許可するか決める。
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix のバブリング段階の pointerdown ハンドラより前に記録する
  );
}

// radixOverlayWasOpenAtPointerDown は「直近の pointerdown 発生時に Radix ポップアップが
// 開いていたか」を返す。ドロワー/ダイアログはこれに基づき、ポップアップが開いているときにオーバーレイをクリックしたら → ポップアップだけ閉じ、自身は閉じない。
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText はテキストをクリップボードに書き込み、成功したかを返す。
// 背景: navigator.clipboard は安全なコンテキスト（HTTPS / localhost）でのみ利用可能。IP + HTTP で
// アクセスした場合は undefined になるため、そのときは execCommand("copy") にフォールバックする。
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // フォールバック処理へ進む
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
