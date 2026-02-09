# 预装 ca-certificates、pyyaml、tzdata、curl，避免启动时 apk/pip
FROM python:3.12-alpine
ENV TZ=Asia/Shanghai
ENV DAILY_UPDATE_TIME=05:00
RUN apk add --no-cache ca-certificates tzdata curl \
    && pip install --no-cache-dir pyyaml
WORKDIR /app
# 统一启动流程：merge_config → gen_custom_rules → 生成 crontab → crond(后台) → updater
RUN printf '%s\n' '#!/bin/sh' \
    'python -u /app/merge_config.py || exit 1' \
    'python -u /app/gen_custom_rules.py || exit 1' \
    'TZ="${TZ:-Asia/Shanghai}"' \
    'TIME="${DAILY_UPDATE_TIME:-05:00}"' \
    'HOUR=$(echo "$TIME" | cut -d: -f1)' \
    'MINUTE=$(echo "$TIME" | cut -d: -f2)' \
    'printf "TZ=%s\\nRULES_DIR=/etc/mosdns/rules\\nMOSDNS_CONFIG_DIR=/etc/mosdns\\nCUSTOM_SOURCE=/app/custom.txt\\n%s %s * * * /app/daily_update.sh\\n" "$TZ" "$MINUTE" "$HOUR" > /etc/crontabs/root' \
    'crond -b -l 2' \
    'exec python -u /app/updater.py' > /entrypoint.sh && chmod +x /entrypoint.sh
# daily_update.sh 需拷入镜像（compose 中 /app 可能只读挂载）
COPY daily_update.sh /app/daily_update.sh
RUN chmod +x /app/daily_update.sh
CMD ["/entrypoint.sh"]
