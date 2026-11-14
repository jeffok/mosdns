# mosdns 家庭 DNS，含 ros_addrlist 插件（海外应答 IPv4 → RouterOS address-list）。
# 本地构建 mosdns 二进制以拼入本仓库插件；不依赖预编译镜像。

FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS mosdns-build
RUN apk add --no-cache git ca-certificates && rm -rf /var/cache/apk/*
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=sum.golang.google.cn \
    CGO_ENABLED=0 \
    GOTOOLCHAIN=local

WORKDIR /src
COPY tools/build-mosdns.sh /src/tools/build-mosdns.sh
COPY plugins/ros_addrlist /src/plugins/ros_addrlist
# 复用本机已 clone 的上游（容器内访问 GitHub 不稳定）
COPY .cache/mosdns-upstream /src/.cache/mosdns-upstream
ARG MOSDNS_REF=v5.3.4
ARG MOSDNS_COMMIT=b7323188bab1ea742538aeccb31b692bc4967d1b
RUN sh /src/tools/build-mosdns.sh prepare
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    OUT=/out/mosdns sh /src/tools/build-mosdns.sh build

FROM alpine:3.21
RUN apk add --no-cache tzdata ca-certificates wget \
 && rm -rf /var/cache/apk/* /tmp/*
COPY --from=mosdns-build /out/mosdns /usr/local/bin/mosdns
COPY scripts/config.template.yaml /opt/mosdns/
COPY scripts/entrypoint.sh /entrypoint.sh
COPY scripts/sync-ai.sh /sync-ai.sh
COPY rules/ /opt/mosdns-rules/

ARG CACHEBUST
RUN chmod +x /entrypoint.sh /sync-ai.sh \
 && for u in \
      "direct-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt" \
      "china_ip_list.txt|https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt" \
      "apple-cn.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt" \
      "proxy-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt" \
      "geosite-gfw.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt" \
    ; do \
      f="${u%%|*}"; url="${u#*|}"; \
      wget -q --timeout=15 --tries=2 -O "/opt/mosdns-rules/$f" "$url" || true; \
    done

ENTRYPOINT ["/entrypoint.sh"]
