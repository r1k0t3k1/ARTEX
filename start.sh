#!/bin/sh
# ARTEX デーモン起動スクリプト（Linux / macOS / Docker ENTRYPOINT）
#
# 使い方：
#   ./start.sh                       フォアグラウンドで実行（Ctrl-C で停止）
#   nohup ./start.sh >artex.log 2>&1 &   バックグラウンドで常駐
#   ./start.sh -addr :9000           追加引数はそのまま artex に渡す
#
# やることは 1 つだけ：artex を起動し、プロセスが終了したら終了コードに応じて再起動するか決める。
#
#   0      ユーザーによる正常停止        → ループを抜ける
#   75     プログラムからの再起動要求        → 即座に再実行（ページで「ワンクリック更新」または「ロールバック」を押した）
#   その他   クラッシュ                → バックオフして再実行（1→2→4…最大 60 秒）
#
# ダウンロード、SHA256 検証、差し替えをここで行わないのは意図的：そのロジックは sh と bat で
# 2 通り書く必要があり、しかも最も失敗してはならない部分だから。起動できないバイナリに差し替えて
# しまうと、本スクリプトは忠実にそれを何度も起動し続け、ユーザーはマシンに入って手作業で救うしかない。
# そのため検証/差し替えはすべて Go 側（selfupdate パッケージ）に残し、artex 自身が起動時に
# 完了させる。スクリプトはあえて単純に保つ。
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 実行ファイル $BIN が見つかりません" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 停止シグナルを artex 本体へ転送する。
#
# Docker 下ではこれが必須：docker stop は SIGTERM を PID 1（つまり本スクリプト）にしか送らず、
# 子プロセスには送らない。転送しないと artex はシグナルを受け取れず、優雅なシャットダウンが
# できずに 10 秒後に SIGKILL で強制終了され、実行中のタスクが途中で切れてしまう。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# シグナルは wait を中断させ、>128 を返す。このとき子プロセスはまだ優雅なシャットダウン中なので、
	# 本当の終了コードを得るにはもう一度 wait する必要がある。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 停止しました"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常終了"
			exit 0
			;;
		"$RESTART_CODE")
			# 更新/ロールバックの準備完了：再実行後、artex が起動時に差し替えを完了する（selfupdate.Bootstrap 参照）。
			echo "[artex] 再起動を要求（新バージョンを適用）…"
			delay=1
			;;
		*)
			echo "[artex] 異常終了 (code=$code)、${delay}s 後に再起動" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
