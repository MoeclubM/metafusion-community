import type { Metadata } from "next";
import "./globals.css";
import { Providers } from "@/components/Providers";

const themeScript = `(function () {
  try {
    var mode = localStorage.getItem("metafusion_theme_mode") || "dark";
    var effective = mode === "system" ? (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light") : mode;
    document.documentElement.setAttribute("data-theme-mode", effective);
    document.documentElement.style.colorScheme = effective;
  } catch (e) {}
})();`;

// 后台不进搜索引擎：它是运营界面，没有需要被发现的内容。
export const metadata: Metadata = {
  title: "Community Admin · MetaFusion",
  description: "MetaFusion 互动服务运营后台（板块、主题与帖子治理）",
  robots: { index: false, follow: false },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="zh-CN" data-theme-mode="dark" suppressHydrationWarning>
      <head><script dangerouslySetInnerHTML={{ __html: themeScript }} /></head>
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
