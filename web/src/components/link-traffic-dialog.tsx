"use client";

import * as React from "react";

import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { Finding, FindingsPage } from "@/lib/types";

export function LinkTrafficDialog({
  trafficIds,
  onClose,
  onBound,
}: {
  trafficIds: string[];
  onClose: () => void;
  onBound: () => void;
}) {
  const [query, setQuery] = React.useState("");
  const [page, setPage] = React.useState(1);
  const [data, setData] = React.useState<FindingsPage | null>(null);
  const [selected, setSelected] = React.useState<Finding | null>(null);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [loading, setLoading] = React.useState(true);
  React.useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    const timer = setTimeout(() => {
      api
        .findingsPage({ page, pageSize: 20, query })
        .then((d) => {
          if (active) setData(d);
        })
        .catch((e: Error) => {
          if (active) setError(e.message);
        })
        .finally(() => {
          if (active) setLoading(false);
        });
    }, 250);
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [query, page]);
  async function save() {
    if (!selected?.finding_id) return;
    setBusy(true);
    setError("");
    try {
      await api.bindFindingTraffic(
        selected.finding_id,
        trafficIds.map((traffic_id) => ({ traffic_id })),
      );
      toast.success(`${trafficIds.length} 件のトラフィックを脆弱性 #${selected.finding_id} に関連付けました`);
      onBound();
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onClose();
      }}
    >
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>脆弱性に関連付け</DialogTitle>
          <DialogDescription>
            選択した {trafficIds.length} 件のトラフィックを既存の脆弱性の証拠として保存する。
          </DialogDescription>
        </DialogHeader>
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="link-finding-query">脆弱性を検索</FieldLabel>
            <Input
              id="link-finding-query"
              placeholder="名前 / 概要 / 脆弱性カテゴリ"
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setPage(1);
              }}
            />
          </Field>
        </FieldGroup>
        {error ? (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <div className="flex max-h-[40vh] flex-col gap-2 overflow-auto" aria-busy={loading}>
          {(data?.items ?? []).map((f) => (
            <Button
              key={f.finding_id ?? f.id}
              variant={selected?.finding_id === f.finding_id ? "secondary" : "outline"}
              className="h-auto justify-start p-3 text-left whitespace-normal"
              aria-pressed={selected?.finding_id === f.finding_id}
              disabled={busy || !f.finding_id || f.inherited}
              onClick={() => setSelected(f)}
            >
              <span className="flex min-w-0 flex-col gap-1">
                <span>
                  #{f.finding_id ?? f.id} · {f.name || f.vulnclass}
                </span>
                <span className="line-clamp-2 text-xs text-muted-foreground">{f.summary}</span>
              </span>
            </Button>
          ))}
          {!data?.items.length ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              {loading ? "読み込み中…" : "一致する脆弱性がない。先に脆弱性を登録してください"}
            </p>
          ) : null}
        </div>
        <div className="flex items-center justify-between gap-2">
          <Button variant="outline" size="sm" disabled={loading || page <= 1} onClick={() => setPage((p) => p - 1)}>
            前へ
          </Button>
          <span className="text-xs">
            {page} ページ目 · 全 {data?.total ?? 0} 件
          </span>
          <Button
            variant="outline"
            size="sm"
            disabled={loading || page * 20 >= (data?.total ?? 0)}
            onClick={() => setPage((p) => p + 1)}
          >
            次へ
          </Button>
        </div>
        <p className="text-sm">
          {selected
            ? `選択中の脆弱性：#${selected.finding_id} ${selected.name || selected.vulnclass}`
            : "脆弱性を選択してください"}
        </p>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={onClose}>
            キャンセル
          </Button>
          <Button disabled={busy || !selected} onClick={() => void save()}>
            {busy ? "保存中…" : "関連付けを確定"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
