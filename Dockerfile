FROM irinesistiana/mosdns:v5.3.3
RUN apk add --no-cache tzdata openssh-client sshpass
RUN mkdir -p /opt/mosdns-configs /opt/mosdns-rules
COPY configs/*.yaml /opt/mosdns-configs/
COPY rules/custom-local.txt rules/custom-hosts.txt rules/ai-list.txt /opt/mosdns-rules/
COPY entrypoint.sh /entrypoint.sh
COPY sync-ai.sh /sync-ai.sh
RUN chmod +x /entrypoint.sh /sync-ai.sh
ENTRYPOINT ["/entrypoint.sh"]
