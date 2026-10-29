"use client";

// LLM リトライ設定の共通部品：5 層のリトライそれぞれの「回数 + 間隔」。
//
// 5 層は内側から外側へ：接続確立（SDK）→ 空応答（SDK）→ 同一 provider の安全ウィンドウ → ポーリング遮断 → 意図の再実行。
// 最初の 3 層はエンドポイントに紐づくため、各モデル設定でグローバル既定を上書きできる。後の 2 層はプロセス単位で、グローバルに 1 つだけ。
//
// すべての入力は「空欄 = 未設定」という共通の意味論に従い、バックエンドの db.RetryRule と一致する：
//   回数   空/0 = 組み込み既定を使用 | -1 = この層のリトライを無効化 | >0 = この回数を使用
//   間隔   空/0 = この層本来の指数バックオフを使用 | >0 = この固定ミリ秒間隔に変更

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** この層のリトライがどこで、誰によって実行されるか */
  where: string;
  /** どのようなエラーがこの層に到達するか。ステータスコードまで具体的に示し、推測させない */
  trigger: string;
  /** 似ているが【この層を通らない】エラー。設定しても反応がなく、バグだと誤解されるのを防ぐ */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 回数が空欄のときの既定値。プレースホルダー用 */
  defAttempts: number;
  /** 間隔が空欄のときの既定ポリシー。プレースホルダー用 */
  defInterval: string;
  /** 回数に -1 を入れた場合の意味 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "接続確立リトライ",
    where: "SDK · 200 を受け取るまで",
    trigger:
      "接続できない、またはまだ 200 を受け取っていない場合：接続リセット / 読み書きタイムアウト / DNS 失敗などのネットワーク層エラー、および HTTP 408、429、500、502、503、504。",
    skips:
      "それ以外のステータスコード（400 / 401 / 403 / 404 / 413 / 422 など）は決定的な拒否で、再送しても同じように失敗するためそのまま上位へ伝播する。",
    desc: "同じリクエストをそのまま再送する。ストリームが始まった後（200 を受け取った後）の途中切断はこの層の管轄外。",
    attemptsLabel: "リトライ回数",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1 = 一度もリトライせず、失敗したら即座に上位へ伝播",
  },
  empty: {
    title: "空応答リトライ",
    where: "SDK · openai 形式のみ",
    trigger:
      "HTTP 200 で finish_reason も正常な stop なのに、応答全体にコンテンツブロックが 1 つもない。ゲートウェイの空フレーム、思考フィールドの欠落、サンプリングの瞬断はいずれもこの形になる。",
    skips:
      "max_tokens による打ち切りで内容がない場合は該当しない（出力上限を上げるべきで、再送しても同じところで切れるだけ）。",
    desc: "プロンプト全体を再送するため長いコンテキストではコストが高く、回数は大きくしない方がよい。",
    attemptsLabel: "リトライ回数",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1 = 空応答をそのまま呼び出し元へ返す",
  },
  stream: {
    title: "同一 provider 安全ウィンドウでのリトライ",
    where: "本プロジェクト · 出力を渡す前",
    trigger:
      "ストリームが確立した後（200 を受け取った後）に問題が発生：接続の途中切断、プロバイダの overloaded、ストリーム内の 429 / 5xx エラーイベント。しかもまだ 1 token も呼び出し元へ渡していない場合。",
    skips:
      "クォータ枯渇（402 / insufficient_quota、ポーリングによる設定切り替えに任せる）、コンテキスト過長（413 / context length、圧縮に任せる）、400 / 401 / 403 / 404 / 422 の決定的な拒否はいずれもリトライしない。",
    desc: "同じ設定で同じリクエストを再生する。まだ出力を一切渡していないため、再生してもモデル出力やツール実行が重複することはない。",
    attemptsLabel: "リトライ回数",
    defAttempts: 2,
    defInterval: "0.5s→1s 指数（上限 4s）",
    offHint: "-1 = ストリーム断をそのまま外側の意図再実行に任せる",
  },
  breaker: {
    title: "ポーリング遮断",
    where: "本プロジェクト · プロセス単位、グローバルに 1 つ",
    trigger:
      "一時的な失敗（429、5xx、ネットワークエラー）が連続してしきい値に達すると遮断する。残高不足（402）、キー失効（401 / 403）、モデル不在（404）のような決定的な失敗はしきい値を見ず、初回で遮断する。",
    skips:
      "1 回成功すればカウントはゼロに戻るため、たまに不調になる程度の設定がじわじわ積み上がって遮断されることはない。",
    desc: "遮断後はクールダウンに入り、クールダウン中はポーリングがこの設定を直接スキップする。状態は DB に保存され、再起動しても失われない。",
    attemptsLabel: "連続失敗何回で遮断",
    defAttempts: 3,
    defInterval: "1min→5min→30min の段階",
    offHint: "-1 = 一時的な失敗では遮断しない（決定的な失敗は依然として遮断）",
  },
  intent: {
    title: "意図の再実行",
    where: "本プロジェクト · プロセス単位、グローバルに 1 つ",
    trigger:
      "手前の層で受け止めきれなかった場合：worker が model_error で終了する。内側のリトライをすべて使い切ったか、ストリームが出力を渡し始めた後に断絶した（その時点での再生は安全でなく、全体をやり直すしかない）場合。",
    skips:
      "クォータ枯渇はポーリングによる設定切り替えで処理済みのため、ここでは再実行しない。タスクが一時停止 / 終了 / 締めくくりに入った場合は即座に譲り、バックオフ時間を消費しない。",
    desc: "意図全体を最初からやり直す。最も外側の層であるため、1 回の再実行は内側の各層の回数がもう一度掛け合わされることを意味する。",
    attemptsLabel: "再実行回数",
    defAttempts: 2,
    defInterval: "固定 3s",
    offHint: "-1 = 再実行せず、その意図は直接 blocked と判定される",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** ミリ秒を読みやすくした文字列。入力欄の横での表示用で、ゼロを数えずに済むようにする。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 制御された数値入力：空文字列 ↔ 0。途中状態（"-"、"1e"）はローカルにそのまま保持し、親を煩わせない。 */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 親が値のセット全体を差し替えたとき（ポリシー読込、設定切り替え）に追随する。自分で入力している
  // ときはここに来ない。そのとき value はローカルテキストを parse した結果と既に等しいため。
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 1 層分のリトライの 2 つのつまみ。idPrefix は同一ページに複数回現れるときに label の htmlFor を保つために使う。 */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 設定ドロワー内のコンパクト版。展開説明を省き、「どのエラーがこの層に来るか」の 1 文だけ残す */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* どのエラーがこの層に来るか、ステータスコードまで具体的に。設定したのに効果が見えないのは、そもそもエラーがこの層に落ちていないことが多い。 */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">トリガー</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">この層を通らない</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`既定 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            間隔 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="既定のバックオフ"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `固定 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">空欄 = 既定を使用。{meta.offHint}。</p>}
    </div>
  );
}

/** モデル設定ドロワー内の 3 層上書き（エンドポイントに紐づく 3 層）。 */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">リトライ上書き</Label>
        <p className="text-muted-foreground text-xs">
          この設定にのみ適用され、「リトライとバックオフ」のグローバル既定を上書きする。各欄が空欄 =
          グローバルに従う。回数 -1 = この層のリトライを無効化。
          間隔を入力すると指数バックオフの代わりに固定間隔を使う。遮断と意図の再実行はプロセス単位のため、グローバルのページでのみ調整できる。
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「リトライとバックオフ」タブ：5 層のグローバル既定値。 */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`リトライポリシーの読み込みに失敗しました：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // バックエンドは範囲外の値を区間内に丸めて返すため、返却値をそのまま使って更新する。見えたものがそのまま保存される。
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("保存しました。即時反映されます（実行中の今回の呼び出しは旧パラメータのまま）");
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> リトライポリシーを読み込み中…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        モデル呼び出し 1 回の失敗は、内側から外側へ 5 層のリトライを順に通る：
        <span className="text-foreground">
          {" "}
          接続確立 → 空応答 → 同一 provider 安全ウィンドウ → ポーリング遮断 → 意図の再実行
        </span>
        。内側を使い切ってから外側に回るため、回数は
        <span className="text-foreground">掛け算</span>
        になる。各層を最大まで上げると、一度の不調で数十回のリクエストを消費しうる。
        すべて空欄なら現在の既定値となり、このページがなかった頃の挙動と完全に同じ。最初の 3
        層は各モデル設定で個別に上書きできる。
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          保存
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          すべて既定に戻す
        </Button>
      </div>
    </div>
  );
}
