import type { Config } from "tailwindcss";

// 只覆盖后台用到的色板：暗色底 + 主色/危险色，避免把整站主题搬过来。
const config: Config = {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        surface: "var(--admin-surface)",
        panel: "var(--admin-panel)",
        line: "var(--admin-line)",
        ink: "var(--admin-ink)",
        muted: "var(--admin-muted)",
        accent: "var(--admin-accent)",
        danger: "#ef5350",
      },
    },
  },
  plugins: [],
};

export default config;
