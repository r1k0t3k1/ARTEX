"use client";

import * as React from "react";

import { Card, CardContent } from "@/components/ui/card";
import { ExplorationGraph } from "@/components/exploration-graph";
import { api } from "@/lib/api";
import type { Edge, TaskNode } from "@/lib/types";

export function GraphTab({ taskId }: { taskId: string }) {
  const [nodes, setNodes] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  // 前回のグラフデータのシグネチャ。ポーリングで同じデータが来たときは setState をスキップし、
  // グラフ全体の無駄な再構築を避ける(ドラッグ中に 20 秒ポーリングで中断されてカクつかないように)。
  // 描画に影響するフィールドだけを使う。
  const sigRef = React.useRef("");

  React.useEffect(() => {
    let cancelled = false;
    sigRef.current = ""; // タスク切り替え:次の更新を強制する
    const load = () => {
      api
        .explorationGraph(taskId)
        .then((g) => {
          if (cancelled) return;
          const ns = g.nodes ?? [];
          const es = g.edges ?? [];
          const sig = JSON.stringify([
            ns.map((n) => [n.id, n.type, n.state, n.priority, n.payload]),
            es.map((e) => [e.src, e.dst, e.rel]),
          ]);
          if (sig === sigRef.current) return; // 変化なし → 再構築しない
          sigRef.current = sig;
          setNodes(ns);
          setEdges(es);
        })
        .catch(() => {
          /* 最後に取得できた正常なデータを保持する */
        });
    };
    load();
    const timer = setInterval(load, 20000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId]);

  return (
    <Card>
      <CardContent className="p-0">
        <ExplorationGraph nodes={nodes} edges={edges} className="h-[72vh]" />
      </CardContent>
    </Card>
  );
}
