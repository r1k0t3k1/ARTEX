"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // 条項の最下部までスクロールした場合（スクロール不要で全体が表示される場合を含む）
  // に限り「同意する」をクリックできる。
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 開いたときにリセットし、内容がもともと 1 画面に収まりスクロールが発生しない
    // ケースにも対処する。
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // ログイン済みならそのままメイン画面へ（静的エクスポートでは middleware が
    // このリダイレクトを行わない）。
    const token = auth.getToken();
    if (token) {
      // localStorage に資格情報が残っていても cookie は失われている場合がある。
      // まず同期し、その後で新規リクエストを発行する。そうしないとサーバー側の
      // ガードやルートキャッシュが、checking 状態のままのログインページへ
      // リダイレクトを戻してしまう。
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("バックエンドサービスに接続できません"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("先に「利用規約」を読んで同意してください");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("ユーザー名またはパスワードが正しくありません");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        ログイン状態を確認しています…
      </div>
    );
  }

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">ログイン</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">
              おかえりなさい。ARTEX を引き続き利用するにはパスワードを入力してください
            </p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">ユーザー名</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">パスワード</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="パスワードを入力"
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <div className="flex items-start gap-2">
              <Checkbox
                id="agree-terms"
                checked={agreed}
                onCheckedChange={(v) => setAgreed(v === true)}
                className="mt-0.5"
              />
              <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
                以下を読み、同意します
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  「利用規約」
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "ログイン中..." : "ログイン"}
            </Button>
          </form>
        </div>
      </div>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">ARTEX 利用規約と免責事項</DialogTitle>
              <p className="text-xs text-muted-foreground">
                バージョン v1.0 · 施行日 2026-09-18 · ログイン前に以下の全条項を最後までお読みください
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              本「利用規約と免責事項」（以下「本規約」）は、本ソフトウェアの使用に関して、お客様と ARTEX
              プロジェクトの作者および貢献者との間で成立する合意です。ご使用前に各条項を慎重に読み、十分に理解してください。特に、太字または色付きで示された免責、責任制限、禁止事項の条項にご注意ください。
              <span className="font-medium text-foreground">
                {" "}
                本ソフトウェアをダウンロード、インストール、アクセス、またはその他いかなる方法で使用した時点で、本規約のすべての拘束に読み、理解し、同意したものとみなされます。
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                第 1 条 · 定義とオープンソースライセンス
              </h4>
              <p className="pl-7">
                本ソフトウェア（ARTEX）は GNU Affero General Public License
                v3.0（AGPL-3.0）で公開されたオープンソースプログラムです。本ライセンスに基づき、自由に使用、複製、変更、配布できます。ただし、いかなる派生作品（サードパーティへオンラインサービスとして提供する場合を含む）も同様に
                AGPL-3.0 でオープンソース化し、利用者へ対応する完全なソースコードを公開する必要があります。AGPL-3.0
                の完全な条項は同梱の LICENSE ファイルに従います。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                第 2 条 · 許可される使用範囲
              </h4>
              <p className="pl-7">
                本ソフトウェアは、個人の学習、コードの研究、セキュリティ技術の原理の検討、およびご自身で構築したローカルの隔離環境での技術検証のみに使用できます。学習、学術研究、コードレビューなどの非攻撃的・非破壊的な用途に適しています。本条で明示的に許可された場合を除き、その他のいかなる目的にも使用してはなりません。
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />第 3 条 · 禁止行為
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  いかなるウェブサイト、オンラインサービス、他者またはサードパーティが所有するネットワーク接続システムに対しても、スキャン、探査、エクスプロイト、攻撃を行ってはなりません（許可の有無、自己資産かどうかを問いません）。
                </li>
                <li>
                  本ソフトウェアを実際のペネトレーションテスト、攻防対抗、レッド/ブルーチーム演習、または本番環境で使用してはなりません。
                </li>
                <li>
                  本ソフトウェアを不正侵入、データ窃取、恐喝、サービス拒否（DoS/DDoS）、その他破壊的・犯罪的な活動に使用してはなりません。
                </li>
                <li>
                  本ソフトウェアおよびその出力に含まれる著作権、ライセンス、セキュリティ上の表示を削除、改ざん、回避してはなりません。
                </li>
                <li>お客様の所在する国または地域の法令、法規、監督規定に違反する行為を行ってはなりません。</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                第 4 条 · 知的財産権
              </h4>
              <p className="pl-7">
                本ソフトウェアの著作権および関連する知的財産権はプロジェクトの作者および貢献者に帰属し、AGPL-3.0
                で定められた範囲でお客様に対応する権利を許諾します。同ライセンスで明示的に許諾された権利を除き、本規約は明示・黙示を問わずその他の権利を許諾するものではありません。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                第 5 条 · データとプライバシー
              </h4>
              <p className="pl-7">
                本ソフトウェアは自己ホスト可能なオープンソースプログラムであり、作者は集中的なサービスを運営せず、お客様の使用データを収集・送信しません。使用中に生成、処理、または接触したすべてのデータはお客様自身が管理し、その合法性と安全性に責任を負います。データの不適切な処理に起因するいかなる結果もお客様の負担となります。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                第 6 条 · コンプライアンスと法的責任
              </h4>
              <p className="pl-7">
                お客様は、所在する国または地域のサイバーセキュリティ、データセキュリティと個人情報保護、コンピュータ犯罪などに関するすべての法令を自ら遵守する必要があります（中国大陸では「サイバーセキュリティ法」「データセキュリティ法」「個人情報保護法」および関連する司法解釈を含みますがこれらに限りません）。
                <span className="font-medium text-foreground">
                  {" "}
                  上記の法令または本規約に違反することにより生じるすべての法的責任と結果は、お客様が単独で負い、本ソフトウェアの作者および貢献者とは無関係です。
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                第 7 条 · 免責事項と責任制限
              </h4>
              <p className="pl-7">
                本ソフトウェアは「現状のまま（AS IS）」および「提供可能な範囲（AS
                AVAILABLE）」で提供され、商品性、特定目的への適合性、正確性、非侵害性の保証を含め、明示・黙示を問わずいかなる保証も伴いません。適用法が許す最大限の範囲で、本ソフトウェアの作者および貢献者は、本ソフトウェアの使用または使用不能（使用方法の適否を問わない）により生じたいかなる直接的、間接的、偶発的、特別、または結果的損害についても責任を負いません。これにはデータの喪失、システムの破損、業務の中断、利益の損失、法的紛争などが含まれますがこれらに限りません。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                第 8 条 · 条項の変更と最終解釈
              </h4>
              <p className="pl-7">
                作者は法令またはプロジェクトの発展に応じて本規約を随時更新する権利を有します。更新版はプロジェクトとともに公開され、公布日から効力を生じます。お客様が本ソフトウェアの使用を継続した場合、改訂後の条項に同意したものとみなされます。法律が許す範囲で、本規約の最終解釈権はプロジェクトの作者に帰属します。本規約のいずれかの条項が無効と判断されても、他の条項の効力には影響しません。
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "すべての条項を閲覧しました" : "条項を最下部までスクロールしてから確認してください"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                すべての条項を読み、同意します
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
