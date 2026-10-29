import { fileURLToPath } from "node:url";

// 静的エクスポート：`NEXT_EXPORT=1 next build` で純粋な静的ディレクトリを web/out に生成し、
// そのまま nginx の web ルートに置いて配信できる。開発時（next dev）はこの変数を設定せず、/api のリバースプロキシと
// ホットリロードを維持する。
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel デモ：サイト全体を mock で動作させ、バックエンドなし、/api のリバースプロキシ不要。
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 親ディレクトリの lockfile がルート推論やアセットパス生成に影響しないようにする。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 開発リソース（HMR）への LAN 内 IP からのアクセスを許可する（必要に応じて増減）。
  // 開発段階では任意の IPv4 ソースからの /_next/* と HMR へのアクセスを開放（LAN 内 IP が変わっても影響なし）。
  // 注意：Next はセキュリティ上裸の "*" を禁止しており、セグメント別のワイルドカードが必要で、"*.*.*.*" は任意の IPv4 に一致する。
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 純粋な静的エクスポート：Node ランタイムなし。画像は最適化しない。各ルートは <route>/index.html を生成。
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock デモ：バックエンドなし、/api のリバースプロキシ不要。
          images: { unoptimized: true },
        }
      : {
          // 開発：/api/* を Go バックエンドへリバースプロキシ（既定 :8787、AUTOPENTEST_API で上書き可能）。
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
