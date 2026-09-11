# ── 构建阶段：静态编译，CGO 关闭 ──
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
# 先拉依赖（当前零第三方依赖，此步接近空操作，保留以兼容未来加依赖）
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cc-proxy .

# ── 运行阶段：最小镜像，静态二进制 ──
FROM alpine:3.20
RUN adduser -D -H proxy
COPY --from=build /out/cc-proxy /usr/local/bin/cc-proxy
USER proxy
EXPOSE 3050
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --spider -q http://127.0.0.1:3050/health || exit 1
ENTRYPOINT ["cc-proxy"]
