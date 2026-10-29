"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 新バージョンが立ち上がるまでの最大待機時間。1 回のアップグレードでは 3 回の
 *  プロセス起動（ステージング → 差し替え → 新バージョン）を経るが、いずれも
 *  秒単位であり、3 分あれば遅いディスクや Docker コンテナ再作成もカバーできる。 */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress とは分ける：ステージング完了後にプロセスが終了して SSE が切れるため、
  // その時点で /api/health のポーリングへ切り替える必要がある。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet はバックエンドキャッシュを迂回するかどうかも兼ねる：ページ表示時の自動
  // チェックはキャッシュを使い（ヘッダーバーで直前に確認済み）、ユーザーが手動で
  // 「アップデートを確認」を押した場合は強制的に再取得する。そうしないと、公開
  // 直後のバージョンはキャッシュが切れるまで見えない。
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("アップデート確認に失敗しました：" + r.error);
          else if (r.has_update) toast.success(`新しいバージョン ${r.latest} が見つかりました`);
          else if (r.comparable) toast.success("現在は最新バージョンです");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("アップデート確認に失敗しました：" + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // バージョン番号が変わるまで /api/health をポーリングする。
  //
  // 判定条件は「バージョンが変わった」であること。「接続できた」ではない：
  // 差し替え中は旧バージョンが一時的に再起動してくる（その 1 回は artex.new を
  // 差し替えてすぐ終了する役目だけを持つ）ため、疎通だけで判断すると成功を誤認する。
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`${j.version} に更新しました。ページを再読み込みします`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 再起動の時間帯に接続できないのは想定内なので、ポーリングを続行する。
      }
    }
    setRestarting(false);
    toast.error(
      "サービスの再起動待ちがタイムアウトしました。バックエンドのログを確認するか、artex が start.sh / start.bat 経由で起動されているか確認してください。",
    );
  }, []);

  // アップデート進捗を購読する。SSE は Next の /api リライトを通さない（あの層が
  // バッファリングしてイベントが流れない）。
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("更新に失敗しました：" + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // プロセス終了時は SSE が必ず切れる。すでに再起動待ちに入っていれば
        // 正常な挙動なので、/api/health のポーリングに判定を任せればよい。
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${info.latest} に更新しますか？\n\n` +
        "更新によりプログラムが再起動し、実行中のタスクは中断されます。\n" +
        (info.mode === "docker"
          ? "\n注意：コンテナ内の更新はプログラム本体のみを差し替え、イメージ内の playwright / nmap などのツールチェーンは更新されません。" +
            "新バージョンが新しいツールに依存する場合は docker compose pull を使用してください。"
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "準備中…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("更新の開始に失敗しました：" + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "前のバージョンにロールバックしますか？\n\nプログラムが再起動し、実行中のタスクは中断されます。\n注意：データベース構造は巻き戻らないため、旧バージョンが新バージョンの書き込んだデータを認識できない可能性があります。",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("前のバージョンに切り替えました。再起動中…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("ロールバックに失敗しました：" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 実際のパーセントが取れるのはダウンロード段階だけ（Content-Length から算出）。
  // 検証/展開/再起動待ちは所要時間が不明な段階なので、プログレスバーを満タンにして
  // パルスアニメーションを付け、「処理中だがいつ終わるか不明」を表す。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 設定ページはマルチカラムのウォーターフォールレイアウトなので、カード自身が
    // 行間隔を持ち、カラムをまたいで分割されないようにする（page.tsx のコメント参照）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          バージョンとアップデート
        </CardTitle>
        <CardDescription>
          GitHub
          から新バージョンを確認してインストールします。更新によりプログラムが再起動し、実行中のタスクは中断されます。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">現在のバージョン</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "スタンドアロン"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新バージョン</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              変更履歴 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub に接続できません：{info.error}
            {"　"}上部でグローバルプロキシを設定してから再試行してください。
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} には {info.os}/{info.arch} 向けのリリースパッケージが提供されていません（{info.asset}{" "}
            が欠落）。自動更新できません。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            ダウンロードするファイル：<span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}。SHA256
            の検証とスモークテストの後にのみ差し替え、失敗時は現在のバージョンを自動的に維持します。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            現在は最新バージョンです。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker 環境での更新はプログラム本体のみを差し替え、イメージ内の playwright / nmap
            などのツールチェーンは更新されません。また、
            <span className="font-mono"> docker compose up -d </span>
            コンテナを再作成するとイメージ同梱のバージョンに戻ります。イメージごとアップグレードする場合は次を実行してください：
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting
                ? "新バージョンを適用するため再起動中です。しばらくお待ちください（ページは自動的に再読み込みされます）…"
                : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            アップデートを確認
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `${info.latest} に更新` : "今すぐ更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              前のバージョンにロールバック
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          ワンクリック更新は監視スクリプトによる再起動に依存します。<span className="font-mono">start.sh</span>（Windows
          では
          <span className="font-mono"> start.bat</span>）経由で ARTEX を起動してください。artex
          本体を直接実行した場合、プログラム終了後に自動で再起動されません。
        </p>
      </CardContent>
    </Card>
  );
}
