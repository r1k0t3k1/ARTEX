"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * トップバーの「新バージョンあり」通知。ページ全体の読み込み時に一度だけ確認し、
 * 更新があればバージョン番号の横に表示する。クリックでシステム設定ページの
 * 「バージョンと更新」カードへ直接移動する。
 *
 * バックエンドは GitHub への問い合わせ結果を 30 分キャッシュしているため、
 * ここでマウントのたびに確認しても安全。未認証の GitHub API は 1 時間/IP あたり
 * 60 回しか使えず、そのキャッシュがなければタブをいくつか開くだけでクォータを
 * 使い切ってしまい、いざ更新したいときに確認できなくなる。
 *
 * 問い合わせ失敗はすべて無視する。トップバーはエラーを出す場所ではない。
 * ユーザーが設定ページで「アップデートを確認」を押せば理由が表示される。
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update には「バージョン番号が比較可能か」の判定も含まれるため、
        // 開発ビルドではこの通知は表示されない。
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 無視する。ネットワーク不通 / GitHub のレート制限でトップバーに
        // エラーを出してはいけない。
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`新バージョン ${latest} が見つかりました。クリックで更新へ`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 点滅するドット：トップバーには要素が多く、文字だけでは見落とされやすいため、
          アニメーションで一目で分かるようにする。 */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">新バージョン {latest}</span>
      <span className="sm:hidden">新バージョン</span>
    </Link>
  );
}
