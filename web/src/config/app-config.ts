import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "ARTEX",
  version: packageJson.version,
  copyright: `© ${currentYear}, ARTEX.`,
  meta: {
    title: "ARTEX — 自律ペネトレーションテストコンソール",
    description: "LLM 駆動の自律ペネトレーションテストシステムのコンソール",
  },
};
