#!/usr/bin/env bash
# 開発モード：バックエンド(:8787) + トラフィックプロキシ(:8788) と フロントエンド next dev(:5173) を一緒に実行する。
# フロントエンドの /api はバックエンドへリバースプロキシ。Ctrl-C でまとめて終了する。
#
# 単一バイナリ（フロントエンド埋め込み）方式は README の「単一バイナリ」の節を参照。このスクリプトは使わない。
set -euo pipefail
cd "$(dirname "$0")"

# 終了時にこのプロセスグループ内のすべての子プロセス（バックエンド + フロントエンド）を終了する。
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# バックエンド（通常の go run、フロントエンドは埋め込まない）。並列な work agent 数は「システム設定」で設定する。
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# フロントエンドのホットリロード（Vite/Next dev server、/api は :8787 へリバースプロキシ）。
( cd web && npm run dev ) &

echo "[dev] バックエンド :8787 / プロキシ :8788 / フロントエンド http://localhost:5173  (Ctrl-C で終了)"
wait
