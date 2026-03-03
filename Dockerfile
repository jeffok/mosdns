FROM irinesistiana/mosdns:v5.3.3
RUN apk add --no-cache tzdata openssh-client sshpass
RUN mkdir -p /opt/mosdns-configs /opt/mosdns-rules
COPY configs/*.yaml /opt/mosdns-configs/
COPY rules/custom-local.txt rules/custom-hosts.txt rules/ai-list.txt /opt/mosdns-rules/
RUN wget -q -O /opt/mosdns-rules/direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt" \
 && wget -q -O /opt/mosdns-rules/china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt" \
 && wget -q -O /opt/mosdns-rules/apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt" \
 && wget -q -O /opt/mosdns-rules/proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt" \
 && wget -q -O /opt/mosdns-rules/geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt" \
 || true
COPY entrypoint.sh /entrypoint.sh
COPY sync-ai.sh /sync-ai.sh
RUN chmod +x /entrypoint.sh /sync-ai.sh
ENTRYPOINT ["/entrypoint.sh"]
