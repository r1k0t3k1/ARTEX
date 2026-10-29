// Mock スイッチ。ビルド時に注入される公開変数（NEXT_PUBLIC_ プレフィックスのみブラウザから読める）。
// Vercel で NEXT_PUBLIC_MOCK=1 を設定するとサイト全体が mock で動作し、バックエンド不要になる。
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
