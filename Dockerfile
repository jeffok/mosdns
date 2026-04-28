FROM irinesistiana/mosdns:v5.3.3

RUN apk add --no-cache tzdata \
 && rm -rf /var/cache/apk/* /tmp/*

COPY scripts/config.template.yaml /opt/mosdns/
COPY scripts/entrypoint.sh /entrypoint.sh
COPY scripts/sync-ai.sh /sync-ai.sh
COPY rules/ /opt/mosdns-rules/

# 引入自动构建缓存参数：确保每周定时构建时强制重新执行后续指令
ARG CACHEBUST

RUN chmod +x /entrypoint.sh /sync-ai.sh \
 && (wget -q -O /opt/mosdns-rules/direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt"   || true) \
 && (wget -q -O /opt/mosdns-rules/china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt" || true) \
 && (wget -q -O /opt/mosdns-rules/apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt"      || true) \
 && (wget -q -O /opt/mosdns-rules/proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt"    || true) \
 && (wget -q -O /opt/mosdns-rules/geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt"           || true)

ENTRYPOINT ["/entrypoint.sh"]
