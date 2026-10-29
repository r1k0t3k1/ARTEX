@echo off
rem コンソールを UTF-8 に切り替える。そうしないと本ファイル内の日本語が GBK 端末では文字化けする。
chcp 65001 >nul 2>&1
rem ARTEX デーモン起動スクリプト（Windows）
rem
rem 使い方：
rem   start.bat                  フォアグラウンドで実行（Ctrl-C で停止）
rem   start.bat -addr :9000      追加引数はそのまま artex に渡す
rem
rem やることは 1 つだけ：artex.exe を起動し、プロセスが終了したら終了コードに応じて再起動するか決める。
rem
rem   0      ユーザーによる正常停止     -> ループを抜ける
rem   75     プログラムからの再起動要求 -> 即座に再実行（ページで「ワンクリック更新」または「ロールバック」を押した）
rem   その他   クラッシュ             -> バックオフして再実行（1->2->4…最大 60 秒）
rem
rem ダウンロード、SHA256 検証、差し替えはすべて artex 自身が起動時に完了する
rem （selfupdate パッケージ）。スクリプトはあえて単純に保つ。詳細は start.sh 冒頭の説明を参照。

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 実行ファイル %BIN% が見つかりません 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 正常終了
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem 更新/ロールバックの準備完了：再実行後、artex が起動時に差し替えを完了する。
	echo [artex] 再起動を要求（新バージョンを適用）…
	set /a delay=1
	goto loop
)

echo [artex] 異常終了 ^(code=!code!^)、!delay!s 後に再起動 1>&2
rem timeout はリダイレクトされたコンソールでは失敗するので、ping で代用する（N 秒の待機には N+1 回必要）。
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
