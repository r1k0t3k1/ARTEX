"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpRightIcon, ChevronLeftIcon, ChevronRightIcon } from "lucide-react";

import { FindingRetestPanel } from "@/components/finding-retest-panel";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { api } from "@/lib/api";
import type { Finding, FindingsPage } from "@/lib/types";

const PAGE_SIZE = 20;

function findingLabel(finding: Finding) {
  return finding.name?.trim() || finding.vulnclass.trim() || "未分類";
}

export function RetestsTab({ taskId }: { taskId: string }) {
  const [page, setPage] = React.useState(1);
  const [data, setData] = React.useState<FindingsPage | null>(null);
  const [selectedId, setSelectedId] = React.useState("");
  const [error, setError] = React.useState("");
  const [refreshVersion, setRefreshVersion] = React.useState(0);
  const refresh = React.useCallback(() => setRefreshVersion((version) => version + 1), []);

  // このタスク自身の検出結果のみ(グラフノードを持たないものも含む)。
  // 履歴は選択中の項目についてのみ取得し、各ポーリングの完了を待つ。
  React.useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function load() {
      try {
        const result = await api.findingsPage({ task: taskId, page, pageSize: PAGE_SIZE, sort: "time" });
        if (disposed) return;
        const lastPage = Math.max(1, Math.ceil(result.total / PAGE_SIZE));
        if (page > lastPage) {
          setPage(lastPage);
          return;
        }
        setData(result);
        setSelectedId((current) =>
          result.items.some((finding) => finding.id === current) ? current : (result.items[0]?.id ?? ""),
        );
        setError("");
      } catch (e) {
        if (!disposed) setError((e as Error).message);
      } finally {
        if (!disposed) timer = setTimeout(() => void load(), 3000);
      }
    }
    void refreshVersion; // 再検証の完了後、トリアージを即座に更新する。
    void load();
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [taskId, page, refreshVersion]);

  const loaded = data?.page === page;
  const findings = loaded ? data.items : [];
  const selected = findings.find((finding) => finding.id === selectedId);

  return (
    <div className="flex flex-col gap-4">
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>
            タスクの脆弱性の読み込みに失敗しました：{error}
            <Button variant="outline" size="sm" onClick={refresh}>
              再試行
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="grid items-start gap-4 lg:grid-cols-[minmax(16rem,22rem)_minmax(0,1fr)]">
        <Card className="min-w-0">
          <CardHeader>
            <CardTitle>脆弱性を選択{data ? ` · ${data.total}` : ""}</CardTitle>
            <CardDescription>
              このタスクの脆弱性の再検証記録を確認したり、新しい再検証を開始したりできます。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex max-h-[32rem] flex-col overflow-y-auto">
            {!loaded && !error ? <Skeleton className="h-24 w-full" /> : null}
            {findings.map((finding, index) => (
              <React.Fragment key={finding.id}>
                {index > 0 ? <Separator className="my-1" /> : null}
                <Button
                  variant={finding.id === selectedId ? "secondary" : "ghost"}
                  className="h-auto w-full shrink-0 flex-col items-start gap-2 whitespace-normal py-3 text-left"
                  aria-label={`脆弱性を選択：${findingLabel(finding)}`}
                  aria-pressed={finding.id === selectedId}
                  onClick={() => setSelectedId(finding.id)}
                >
                  <span className="line-clamp-2 break-words">{findingLabel(finding)}</span>
                  <span className="flex flex-wrap items-center gap-2">
                    <StatusBadge domain="severity" value={finding.severity} dot />
                    <StatusBadge domain="finding" value={finding.status} dot />
                  </span>
                </Button>
              </React.Fragment>
            ))}
            {loaded && findings.length === 0 ? (
              <Empty>
                <EmptyHeader>
                  <EmptyTitle>再検証できる脆弱性はまだありません</EmptyTitle>
                  <EmptyDescription>
                    このタスクで脆弱性が検出されると、ここから手動で再検証を開始できます。
                  </EmptyDescription>
                </EmptyHeader>
              </Empty>
            ) : null}
          </CardContent>
          {data && data.total > PAGE_SIZE ? (
            <CardFooter className="justify-between gap-2">
              <Button
                variant="outline"
                size="icon-sm"
                aria-label="前のページの脆弱性"
                disabled={page === 1}
                onClick={() => setPage(page - 1)}
              >
                <ChevronLeftIcon />
              </Button>
              <span className="text-muted-foreground text-xs">
                {page} / {Math.ceil(data.total / PAGE_SIZE)} ページ
              </span>
              <Button
                variant="outline"
                size="icon-sm"
                aria-label="次のページの脆弱性"
                disabled={page * PAGE_SIZE >= data.total}
                onClick={() => setPage(page + 1)}
              >
                <ChevronRightIcon />
              </Button>
            </CardFooter>
          ) : null}
        </Card>
        {selected ? (
          <div className="flex min-w-0 flex-col gap-4">
            <div className="flex flex-col gap-2">
              <div className="flex flex-wrap items-start justify-between gap-2">
                <h2 className="min-w-0 flex-1 break-words font-medium">{findingLabel(selected)}</h2>
                <Button asChild variant="ghost" size="sm">
                  <Link href={`/function/findings/detail?id=${selected.finding_id || selected.id}`}>
                    脆弱性の詳細 <ArrowUpRightIcon data-icon="inline-end" />
                  </Link>
                </Button>
              </div>
              <p className="line-clamp-3 break-words text-muted-foreground text-sm">{selected.summary}</p>
            </div>
            <FindingRetestPanel
              key={selected.id}
              findingId={selected.finding_id || selected.id}
              findingName={findingLabel(selected)}
              onCompleted={refresh}
            />
          </div>
        ) : null}
      </div>
    </div>
  );
}
