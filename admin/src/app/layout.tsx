import type { Metadata } from "next";
import "./globals.css";
import { Providers } from "@/components/Providers";

// 后台不进搜索引擎：它是运营界面，没有需要被发现的内容。
export const metadata: Metadata = {
  title: "Community Admin · MetaFusion",
  description: "MetaFusion 互动服务运营后台（板块、主题与帖子治理）",
  robots: { index: false, follow: false },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="zh-CN">
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
