# syntax=docker/dockerfile:1
# AI Proxy — 多平台 AI 账号 → OpenAI 兼容代理 addon
# 依赖内核来自 rockswang/workbuddy-wild（合并 WorkBuddy + TraeWork 双平台上游）。

# ============================================================
# 阶段一：构建 Go 二进制（双平台无头 server + 管理命令 + 登录工具）
# ============================================================
FROM docker.io/library/golang:1.25-alpine AS build
ARG BUILD_ARCH
ENV GOPROXY=https://goproxy.cn,direct
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apk/repositories

# BUILD_ARCH: aarch64 -> arm64, amd64 -> amd64
COPY src /src
RUN cd /src && \
    if [ "$BUILD_ARCH" = "aarch64" ]; then export GOARCH=arm64; else export GOARCH=amd64; fi && \
    echo "building for GOOS=linux GOARCH=$GOARCH" && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/serverd ./cmd/serverd && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ctl ./cmd/ctl && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/logintrae ./cmd/logintrae && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/loginqoder ./cmd/login_qoder

# ============================================================
# 阶段二：运行时镜像
# ============================================================
FROM docker.io/library/alpine:3.20

ARG BUILD_VERSION
ARG BUILD_ARCH

LABEL \
    io.hass.version="${BUILD_VERSION}" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.name="AI Proxy" \
    io.hass.description="WorkBuddy + TraeWork 多账号 OpenAI 兼容代理" \
    io.hass.url="https://github.com/C3H3-AI/ai-proxy"

RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apk/repositories

RUN apk add --no-cache \
        bash \
        curl \
        jq \
        python3 \
        ca-certificates \
        tzdata \
    && adduser -D -u 10001 app \
    && mkdir -p /app /data/auths /data/data \
    && chown -R app:app /app /data

COPY --from=build /out/serverd /app/serverd
COPY --from=build /out/ctl /app/ctl
COPY --from=build /out/login /app/login
COPY --from=build /out/logintrae /app/logintrae
COPY --from=build /out/loginqoder /app/loginqoder
COPY run.sh /run.sh
COPY login_ui.py /app/login_ui.py
RUN chmod a+x /run.sh /app/serverd /app/ctl /app/login /app/logintrae /app/loginqoder /app/login_ui.py

WORKDIR /app

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:7870/healthz || exit 1

EXPOSE 7870

CMD [ "/run.sh" ]