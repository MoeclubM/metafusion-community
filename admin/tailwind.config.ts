import type { Config } from "tailwindcss";

// 只覆盖后台用到的色板：暗色底 + 主色/危险色，避免把整站主题搬过来。
const config: Config = {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        surface: "#0b1020",
        panel: "#131a2c",
        line: "#26304a",
        ink: "#e6ebf5",
        muted: "#93a0bd",
        accent: "#5b8def",
        danger: "#ef5350",
      },
    },
  },
  plugins: [],
};

export default config;
