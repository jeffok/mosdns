# 预装 ca-certificates、pyyaml、tzdata、curl，避免启动时 apk/pip
FROM python:3.12-alpine
ENV TZ=Asia/Shanghai
RUN apk add --no-cache ca-certificates tzdata curl \
    && pip install --no-cache-dir pyyaml
WORKDIR /app
# 每日 5:00 北京时间更新规则并重启 mosdns；crond 需 env 以便 gen_custom_rules 正确写入
RUN printf '%s\n' \
    'TZ=Asia/Shanghai' \
    'RULES_DIR=/etc/mosdns/rules' \
    'MOSDNS_CONFIG_DIR=/etc/mosdns' \
    'CUSTOM_SOURCE=/app/custom.txt' \
    '0 5 * * * /app/daily_update.sh' \
    > /etc/crontabs/root
# 统一启动流程：merge_config → gen_custom_rules → crond(后台) → updater
RUN printf '%s\n' '#!/bin/sh' \
    'python -u /app/merge_config.py || exit 1' \
    'python -u /app/gen_custom_rules.py || exit 1' \
    'crond -b -l 2' \
    'exec python -u /app/updater.py' > /entrypoint.sh && chmod +x /entrypoint.sh
# daily_update.sh 需拷入镜像（compose 中 /app 可能只读挂载）
COPY daily_update.sh /app/daily_update.sh
RUN chmod +x /app/daily_update.sh
CMD ["/entrypoint.sh"]
