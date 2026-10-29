# syntax=docker/dockerfile:1
#
# 実行イメージ（イメージ内ではコンパイルしない）：よく使うツールを入れるだけに留め、**ビルド済みの Linux 単一バイナリ**を配置する。
# バイナリは CI の binaries ジョブがクロスコンパイル（純 Go、QEMU 不要）し、対象アーキテクチャ別に
# ビルドコンテキストの dist/<TARGETARCH>/artex へ置く。これによりマルチアーキテクチャビルドでも arm64 は apt 層の
# エミュレーションだけで済み、Next/Go のコンパイルをエミュレートせずに済むため大幅に速い。
#
# ローカルで手動ビルドする場合は、先にバイナリを自分で用意する：
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# よく使うツール：ripgrep / curl / vim に加え、偵察用の定番ツール一式（必要に応じて増減）。
# Node は NodeSource から 20.x を導入：bookworm 標準の apt nodejs は 18 で、Playwright は >=20 を要求する。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Playwright MCP と CLI を（グローバルに）プリインストールし、実行時に npx でネットワーク取得しないようにする。
# @playwright/mcp：ブラウザ MCP は直接 `npx @playwright/mcp`（グローバル導入済みなので -y/@latest 不要）。
# @playwright/cli：playwright-cli を提供。導入後に --help で実行可能か確認する。
# 続けて playwright（ブラウザ管理用）を導入し、--with-deps で chromium とそのシステム依存をプリセットする。
# これによりコンテナ内の MCP/CLI は初回起動からすぐ使え、ブラウザをネットワーク取得しなくて済む。
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 対応アーキテクチャ向けのビルド済みバイナリ（dist/amd64/artex または dist/arm64/artex）
COPY dist/${TARGETARCH}/artex /app/artex
# デーモン起動スクリプト：プロセス終了後に終了コードに応じて再起動するか決定し、ページ上のワンクリック更新は
# これが差し替えを完了させる。また SIGTERM を artex へ転送する役割も担う——docker stop は PID 1 にしか
# シグナルを送らないため、転送しないと artex は受け取れず、優雅なシャットダウンができず 10 秒後に SIGKILL で強制終了される。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）の永続化ポイント
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
