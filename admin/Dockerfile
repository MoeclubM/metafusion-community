# syntax=docker/dockerfile:1
# 互动服务自带后台（community-admin）：Next.js standalone 产物，容器内监听 3000。
# 网关按前缀 /admin/community 接入（见 README「宿主需要接入的东西」）。
#
# 用 node:22-alpine 而不是 bun 镜像：本应用的依赖、类型检查、测试与构建全部在 npm 链上验证，
# 镜像里不引入第二套包管理器（frontend 用 bun 是它自己的历史选择，不在这里复制）。
FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

FROM node:22-alpine AS builder
WORKDIR /app
# NEXT_PUBLIC_* 会在**构建期**内联进产物，因此只能走 build arg（运行期 env 无效）。
# 留空 = 请求同源网关路径 /api/*（生产形态）。
ARG NEXT_PUBLIC_API_BASE=
ENV NEXT_PUBLIC_API_BASE=$NEXT_PUBLIC_API_BASE
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

FROM node:22-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0
RUN addgroup -g 1001 -S nodejs && adduser -S -u 1001 -G nodejs nextjs
COPY --from=builder /app/public ./public
# standalone 里已经带 node_modules 的裁剪副本与 server.js
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static
USER nextjs
EXPOSE 3000
CMD ["node", "server.js"]
