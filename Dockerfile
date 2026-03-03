FROM irinesistiana/mosdns:v5.3.3

RUN apk add --no-cache tzdata openssh-client sshpass \
 && rm -rf /var/cache/apk/* /tmp/*

COPY scripts/config.template.yaml /opt/mosdns/
COPY scripts/entrypoint.sh /entrypoint.sh
COPY scripts/sync-ai.sh /sync-ai.sh
COPY rules/ /opt/mosdns-rules/

RUN chmod +x /entrypoint.sh /sync-ai.sh \
 && wget -q -O /opt/mosdns-rules/direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt" \
 && wget -q -O /opt/mosdns-rules/china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt" \
 && wget -q -O /opt/mosdns-rules/apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt" \
 && wget -q -O /opt/mosdns-rules/proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt" \
 && wget -q -O /opt/mosdns-rules/geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt" \
 || true

ENTRYPOINT ["/entrypoint.sh"]
