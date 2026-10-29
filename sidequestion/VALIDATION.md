# `/btw` 検証記録

日付：2026-09-10。ブランチ：`codex/btw-side-question`。ベースライン：`8dae851b9b622f2ff2631f332fde9719d0b16fba`。

独立した PostgreSQL テスト DB とデータディレクトリを使用。実際のモデル認証情報は独立したテスト環境にのみ注入し、コードやこの記録には書き込んでおらず、製品の既定モデルも変更していません。Go 1.26.3、norma v0.3.6、Next.js 16.2.9。

実際のモデル対話、返却オブジェクト、エンジニアリング上のアサーション、および Qwen の生のレビューテキストは [validation-2026-09-10.json](validation-2026-09-10.json) に保存されており、API 認証情報は含まれていません。

## エンジニアリングチェック

| 範囲 | 結果 | 根拠 |
| --- | --- | --- |
| 構造化メッセージ、ツールパラメータのディープコピー | 通過 | `TestCheckpointDeepCopyAndBoundaries` |
| 要約 / 圧縮リクエストが上書きしないこと、完全な応答と終端状態の発行、半端な応答の除外 | 通過 | `TestCheckpointDeepCopyAndBoundaries`、`TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 実際のモデルプールメンバーの身元 | 通過 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| ツールのペアリング、20 組の再生、予算による切り詰めと超過エラー | 通過 | `TestBuildRequestCompactionToolPairingAndBudget` |
| メインとサイド質問の並行、双方向のキャンセル分離 | 通過 | ブロッキング Provider、`TestMainSideConcurrencyAndIndependentCancellation` |
| ツール実行なし、ストリーミング / 非ストリーミング、失敗時にも既存の使用量 | 通過 | `TestServiceNoToolsAndUsageOnFailure` |
| 実際の norma ChatAgent + ローカル Read ツール、メイン transcript / アクティビティの分離 | 通過 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`、ストリーミングと非ストリーミングのサブケース |
| 永続化、ページング、冪等性、再起動後の部分回答保持 | 通過 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| クリアと遅延書き込みの競合、親リソースの削除、バージョン比較 | 通過 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker のアーカイブと復元、v1/v2/v3 | 通過 | `TestSideTaskArchiveVersions` |
| 3 種類の親インターフェース、認証、リソースの帰属、Worker の論理削除 | 通過 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`、`TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| ビジーなメインセッションでもサイド質問可能、独立した SSE の再接続 / 切断、キャンセル、クリア | 通過 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 親セッションごとに 1 / 全体で 4 の並列度 | 通過 | 2 つの `TestSideHTTP…` ケース |
| 送信前のスナップショット永続化、再起動後の追加質問、旧セッションでスナップショットを偽造できないこと | 通過 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| キャッシュ中の設定が削除された、またはモデルが変わった後に継続を拒否 | 通過 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| アーカイブ前にキャンセルし、最終回答と使用量の永続化を待つ | 通過 | `TestSideTaskDrainPersistsBeforeArchive` |
| ストリーミングのコンシューマが早期にキャンセルした場合、使用量とサイド質問の帰属を一度だけ記録 | 通過 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 再起動で自動復元された Worker / deadline 実行コンテキストが新しいスナップショットを発行し続ける | 通過 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 関連パッケージの race チェック | 通過 | 下記コマンド |
| TypeScript と本番ビルド | 通過 | `npx tsc --noEmit`、`npm run build` |
| 新規フロントエンドモジュールの Biome | 通過 | `biome check`、新規モジュール 3 つ |

使い捨て可能な独立したデータベースで `ARTEX_PG_DSN` を設定すれば、自動チェックを再現できます（本番 DB を指さないこと）：

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

Go の全体回帰はオールグリーンではありません：`server` パッケージの既存テスト 2 件が一時ディレクトリのクリーンアップ段階で失敗し、いずれも `TempDir RemoveAll … directory not empty` を報告します：

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

上記の未変更ベースラインからソースをエクスポートし、同じ隔離環境で `server` パッケージを再実行しても、この 2 つのクリーンアップ失敗が再現しました。ベースラインの実行ではさらに `TestCoreTaskLifecyclePG` のターゲットノード数アサーション失敗が現れましたが、最終変更後の `server` 回帰にはそのアサーション失敗はありません。その他のパッケージは通過し、今回のサイド質問関連ケースおよび race チェックも通過しました。ベースラインの問題を今回の受け入れ通過として扱っておらず、問題を隠すために既存のアサーションを変更してもいません。

Next.js のビルドは既存の複数 lockfile / workspace root 推論の警告を出力しますが、ビルドは完了し、すべてのページが正常に生成されました。

## ブラウザチェック

Codex In-app Browser を使い、独立したローカル Go サービスと Next.js 開発サーバーに接続しました。デスクトップと 390 × 844 の狭い画面で以下の手動・自動操作を完了し、スクリーンショットとブラウザログを確認しました：

- 通常チャットの実行中に `/btw` を入力すると、メイン内容とサイド質問が同時に表示される。デスクトップのサイドバーも正常。
- 連続して追加質問。サイド質問を停止した後も生成済みの部分を保持。メインフローは継続。
- パネルを閉じてもリクエストは継続し、再度開くと完了した回答が復元される。ページを更新すると空の `/btw` で履歴が復元される。
- 狭い画面の Drawer で入力、ボタン、履歴、閉じる操作が正常で、横方向のオーバーフローがない。
- クリアは確認ダイアログを使い、クリア後は履歴が消え、メイン transcript とスナップショットは保持される。
- タスクの MainAgent と 2 つの Worker でそれぞれ質問して切り替えても、Agent タグと履歴が混線しない。
- ブロッキングなローカルモデルフィクスチャで Worker を実行し続ける。Worker のメイン入力欄から `/btw` を送信し、サイド質問を停止しても Worker はリアルタイム実行と自身の一時停止ボタンを表示し続け、サイド質問は部分回答を保存する。
- ブラウザのエラー / 警告ログは空。

制御可能なフィクスチャは並行処理のタイミングを正確に検証するために使い、実際のモデルの出力速度には依存しません。デバッグ中に 2 回、Worker 実行時のチェックで有効な並行ウィンドウが形成されなかった（タスクが終了済み / 回答が早期終了）ため、フィクスチャを修正してやり直し、通過しました。これらの初回操作は有効な通過とは記録していません。

## 実モデルとの対話

まず `grok-4.6` をプローブ。OpenAI 互換インターフェース `http://127.0.0.1:12580/tingly/openai`。プローブは HTTP 200 で、モデル名 `grok-4.6` と `READY` を返し、所要 2.82 秒。先頭候補が利用可能だったため、Tingly の `glm` や Zhipu の `glm-5.3` のフォールバックチェーンは有効化しませんでした。この 2 つのフォールバックサービスは今回検証していません。

| シナリオ | 実際の結果 |
| --- | --- |
| メインセッション実行中に資産、ゴール、マーカーを質問 | `redhaze.top`、トップページの読み取りとゴールの要約、`BTW-REAL-0910` を返す。サイド質問は完了、16.97 秒 |
| メインセッションがトップページを読み終えた後、ツールの根拠を質問 | WebFetch 200、curl のリダイレクト 301 → 302 → 200、ページタイトルを正しく引用。7.24 秒 |
| サイド質問が Bash でテストファイルの作成を要求 | 実行を拒否し、対象ファイルは作成されず。7.74 秒 |
| 完了後のサイド質問がメインコンテキストを変えない | メイン transcript の SHA-256 がメインのアクティビティ記録と一致。サイド質問のツール実行回数は 0 |
| Go サービスを実際に停止 / 再起動してから追加質問 | 以前の 3 件のサイド質問履歴を保持し、永続化されたスナップショットから資産、マーカー、タイトルに直接回答し、メイン Agent を再実行しなかった |
| 新規セッションで Grok の非ストリーミング設定を使用 | 資産と `ATOMIC-0910` に正しく回答。使用量を返して保存：input 11734、output 138、cache_read 11520 |

資産ケースのメインセッションは WebFetch と Bash/curl で公開トップページを読み取り、ランディングページは `https://id.redhaze.top/home`、タイトルは「红幕科技 RedHaze Group · 全球综合集团门户」。Bash はレスポンスをローカルのテストファイルに一時保存し、リモートへの書き込みは行っていません。この事実は「サイド質問がツールを実行していない」こととは別に検証しています。

メイン transcript のチェックサム：`e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`。

**使用量の制限：** Tingly の Grok ストリーミング応答は usage を返しませんでした。別途 `stream_options.include_usage=true` を直接送って検証したところ、HTTP 200、データフレーム 12 個、usage フレーム 0 個でした。したがってストリーミングテストの 0 は、エンドポイントが使用量を提供していないことを意味し、課金がないと解釈してはなりません。非ストリーミングの使用量、およびフィクスチャの失敗 / キャンセル時の使用量は正しく保存されています。

## Qwen レビュー

レビューモデルは `qwen-flash`、OpenAI 互換インターフェース `https://dashscope.aliyuncs.com/compatible-mode/v1`、HTTP 200。最初の 3 つの実サイド質問対話、メインセッションのツール根拠、およびエンジニアリング上のアサーションを提供。`verdict: accept`、`concerns: []` を返し、回答が資産、マーカー、ページ読み取りの証拠と一致し、サイド質問のツール拒否が制約に適合していると判断しました。レビューの使用量：prompt 6625、completion 312、total 6937。

今回の Qwen レビューの範囲には、後から追加したサービス再起動と非ストリーミングのテストは含まれていません。Qwen の「書き込みなし」という概括は広すぎます：メインセッションの curl は確かにローカルのレスポンス一時ファイルを作成しており、上記で明確に記録しています。並行性、ツール実行ゼロ、transcript の分離はエンジニアリング上のアサーションで判断し、モデルレビューは回答品質の評価を補助するだけです。
