FROM irinesistiana/mosdns:v5.3.3
RUN apk add --no-cache tzdata openssh-client sshpass
COPY entrypoint.sh /entrypoint.sh
COPY sync-ai.sh /sync-ai.sh
RUN chmod +x /entrypoint.sh /sync-ai.sh
ENTRYPOINT ["/entrypoint.sh"]
