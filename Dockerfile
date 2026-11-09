# mosdns v2 运行镜像
#   - 基础镜像锁 digest（v5.3.4；digest 是 multi-arch index，构建时按目标平台自动选择）
#   - 构建期预热 ASN-China 规则：必需文件失败即构建失败；可选文件（cn-exceptions/apple-cn）缺失只告警
FROM irinesistiana/mosdns:v5.3.4@sha256:26bd5326578f6b1121dc8a503b30fd4edeedb41a409442b20e6f186e4386c4a4

RUN apk add --no-cache tzdata \
 && rm -rf /var/cache/apk/* /tmp/*

COPY scripts/ /opt/mosdns/
COPY rules/ /opt/mosdns-rules/
# 规则源：默认与运行时一致（§19.3）；构建机无法访问时可改 SENTINEL_CONFIG 之外的 SOURCE_BASE
ARG SOURCE_BASE=https://gh-proxy.com/https://raw.githubusercontent.com/jeffok/ASN-China/release-files
# 每周重建时强制重新拉规则
ARG CACHEBUST=0

# 必需规则：下载失败或为空 → 构建失败（不留 "|| true" 的静默退化）
RUN set -eu \
 && cd /opt/mosdns-rules \
 && for f in cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt; do \
      echo "fetch $f"; \
      wget -q -T 30 -O "$f.tmp" "$SOURCE_BASE/$f"; \
      test -s "$f.tmp"; \
      mv "$f.tmp" "$f"; \
    done \
 && wc -l cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt

# 可选规则：线上 release-files 可能还没发布，取不到不算失败，但必须留下可加载的文件
RUN set -eu \
 && cd /opt/mosdns-rules \
 && for f in apple-cn.txt cn-exceptions.txt bogus-nxdomain.china.conf ai-domains.txt; do \
      if wget -q -T 30 -O "$f.tmp" "$SOURCE_BASE/$f" && [ -s "$f.tmp" ]; then \
        mv "$f.tmp" "$f"; \
      else \
        rm -f "$f.tmp"; : > "$f"; \
        echo "WARN optional rule $f unavailable at build time" >&2; \
      fi; \
    done \
 && : > /opt/mosdns-rules/custom-hosts.txt

# 规则源条目（ai-domains）缺失时退回仓库内的 ai-list.txt，保证 domain_set 可加载
RUN cd /opt/mosdns-rules && if [ -s ai-domains.txt ]; then :; else cp ai-list.txt ai-domains.txt; fi

# 构建产物同样执行运行时阈值、格式与哨兵校验，拒绝非空但损坏的兜底规则。
RUN sh /opt/mosdns/validate.sh --dir /opt/mosdns-rules

RUN chmod +x /opt/mosdns/*.sh 2>/dev/null || true

ENV MOSDNS_VERSION=2.0.0 TZ=Asia/Shanghai
ENV RULES_DIR=/etc/mosdns/rules \
    STATE_DIR=/data/state

VOLUME ["/etc/mosdns", "/data"]

ENTRYPOINT ["/opt/mosdns/entrypoint.sh"]
