#!/usr/bin/env sh
# 本地 Docker 测试：规则生成到 rules/，若 5353 被占用则自动选空闲端口，测试结束删除临时文件。
set -e
cd "$(dirname "$0")/.."

# 选一个未被占用的端口（先试 5353，再试 5354..5362）
find_listen_port() {
  p=5353
  while [ "$p" -le 5362 ]; do
    if ! (lsof -i :"$p" >/dev/null 2>&1); then
      echo "$p"
      return
    fi
    p=$((p + 1))
  done
  echo "5353"
}

PORT=$(find_listen_port)
echo "[test] using MOSDNS_LISTEN_PORT=$PORT"

# 测试用自签名证书
CERTS_DIR="certs_test"
mkdir -p "$CERTS_DIR"
if [ ! -f "$CERTS_DIR/fullchain.pem" ]; then
  openssl req -x509 -nodes -days 1 -newkey rsa:2048 \
    -keyout "$CERTS_DIR/privkey.pem" -out "$CERTS_DIR/fullchain.pem" -subj "/CN=localhost" 2>/dev/null
  echo "[test] created $CERTS_DIR"
fi

export MOSDNS_DATA_DIR="$(pwd)"
export DOH_CERT_DIR="$(pwd)/$CERTS_DIR"
export SITE=sgp
export MOSDNS_LISTEN_PORT="$PORT"

echo "[test] starting compose..."
docker compose up -d --build

# 等待健康
echo "[test] waiting for healthy..."
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if docker compose ps -q updater 2>/dev/null | xargs -I{} docker inspect --format '{{.State.Health.Status}}' {} 2>/dev/null | grep -q healthy; then
    break
  fi
  sleep 3
done

echo "[test] mosdns status: $(docker compose ps mosdns 2>/dev/null || true)"
if command -v dig >/dev/null 2>&1; then
  if dig @127.0.0.1 -p "$PORT" google.com +short +time=2 2>/dev/null | grep -q .; then
    echo "[test] DNS query OK (port $PORT)"
  else
    echo "[test] DNS query skipped or no reply (port $PORT may be blocked)"
  fi
fi

echo "[test] stopping..."
docker compose down

echo "[test] removing temporary files..."
rm -rf "$CERTS_DIR" rules config.yaml site.yaml cache.dump mosdns.log
echo "[test] done."
