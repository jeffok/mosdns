#!/bin/sh
# 每日 5:00 北京时间执行：更新规则并重启 mosdns 使规则生效
# 由 crond 调用，需设置 TZ=Asia/Shanghai 及 RULES_DIR 等环境变量（见 Dockerfile 中 crontab）

echo "[daily_update] $(date) starting..."
python -u /app/gen_custom_rules.py || exit 1

if [ -S /var/run/docker.sock ]; then
  CONTAINER="${MOSDNS_CONTAINER_NAME:-mosdns}"
  echo "[daily_update] restarting ${CONTAINER}..."
  curl -sS -f -X POST --unix-socket /var/run/docker.sock \
    "http://localhost/containers/${CONTAINER}/restart" || echo "[daily_update] WARN: docker restart failed"
else
  echo "[daily_update] no docker socket, skip mosdns restart (e.g. RouterOS Container)"
fi
echo "[daily_update] done"
