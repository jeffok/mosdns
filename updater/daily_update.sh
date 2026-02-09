#!/bin/sh
# 每日定时更新规则并重启 mosdns 使规则生效
# 由 crond 调用，时间和时区从环境变量 TZ 和 DAILY_UPDATE_TIME 读取（默认 Asia/Shanghai 5:00）

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
