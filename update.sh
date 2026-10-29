#!/usr/bin/env bash
# ARTEX 更新スクリプト：① Docker 更新（新イメージを取得して再構築）  ② ローカルビルド更新（バイナリを再ビルド）
# install.sh と対応：install は初回の導入、update は新バージョンへのアップグレードを担当する。
# DB マイグレーションは手動実行不要——artex は起動のたびに schema.sql を冪等に再実行する
# （ADD COLUMN/CREATE INDEX IF NOT EXISTS を含む）ため「再起動がそのままマイグレーション」。
# データ（pgdata ボリューム、./data、./skills）は影響を受けない。
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 任意：リポジトリを最新コードに同期（compose/スクリプト/ローカルビルドのソースもこれで更新される）───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "git の作業コピーではないため、git pull をスキップします"; return; }
  [ "$(ask '最新コードを取得しますか (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull をファストフォワードできませんでした（ローカルに変更があるか、ブランチが分岐しています）——手動で処理してから再試行してください。今回は現在のコードで続行します"
  fi
}

# ── ① Docker 更新 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose が見つかりません。先に ./install.sh でインストール・デプロイしてください"
  [ -f .env ] || die ".env が見つかりません。先に ./install.sh を実行して初回デプロイを完了してください"

  # 任意：指定バージョンの tag にアップグレード（未入力なら .env の ARTEX_TAG を踏襲、未設定時は latest）
  local tag; tag="$(ask '対象イメージの tag（Enter で .env / latest を踏襲）' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG を ${tag} に設定しました"
  fi

  # artex のみを操作する：postgres は固定の 16-alpine で、一緒にアップグレードする必要はない（取得するだけ
  # 帯域の無駄になり、メジャーバージョンの変更には互換性リスクもある）。artex は depends_on postgres を
  # 宣言しているため、サービス名付きの up では pg が起動していなければ自動で起動し、稼働中ならそのまま
  # 維持され再構築されない。
  info "新しいイメージを取得します（artex のみ）…"
  docker compose pull artex
  info "再構築して起動します（artex の再起動時に schema を自動マイグレーション）…"
  docker compose up -d artex
  ok "更新完了 → http://localhost:8787"
  info "ログの確認：docker compose logs -f artex"
  info "古いイメージの削除（任意）：docker image prune -f"
}

# ── ② ローカルビルド更新 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go が見つかりません（>=1.26）：https://go.dev/dl/"
  [ -f config.json ] || warn "config.json が見つかりません——初回デプロイなら ./install.sh を使ってください"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "フロントエンドの静的成果物を再ビルドします…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "フロントエンドを埋め込んだ単一バイナリを再ビルドします…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm が見つかりません：**フロントエンドを埋め込まない**バックエンドをビルドします（フロントエンドは別途 npm run dev で起動）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "ビルド完了 → ./artex"
  warn "反映するには稼働中の artex プロセスを再起動してください（再起動時に schema を自動マイグレーション）"
}

echo "=============================="
echo "  ARTEX アップデート"
echo "  1) Docker 更新（新イメージを取得して再構築）"
echo "  2) ローカル更新（go で再ビルド）"
echo "=============================="
case "$(ask '選択' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "無効な選択です" ;;
esac
