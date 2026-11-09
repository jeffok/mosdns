#!/bin/sh
# mock_ros.sh — 极简 RouterOS REST 模拟，供 sync-ai 验收。
# 用法：mock_ros.sh [port]
set -u
PORT=${1:-80}
STATE=${MOCK_ROS_STATE:-/tmp/mock-ros}
mkdir -p "$STATE"
: > "$STATE/http.log"
: > "$STATE/added.log"
printf '*1\tai-sgp\t1.2.3.4\tmosdns-ai\n' > "$STATE/entries"
echo 2 > "$STATE/nextid"
printf '%s' "${ROS_USER:-admin}:${ROS_PASS:-testpass}" | base64 | tr -d '\n' > "$STATE/expect.b64"

cat > "$STATE/handler.sh" <<'HS'
#!/bin/sh
set -u
STATE=${MOCK_ROS_STATE:-/tmp/mock-ros}
EXPECT_B64=$(cat "$STATE/expect.b64")

respond() {
  code=$1; text=$2; body=$3
  len=$(printf '%s' "$body" | wc -c)
  printf 'HTTP/1.1 %s %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
    "$code" "$text" "$len" "$body"
}

IFS= read -r req || exit 0
req=$(printf '%s' "$req" | tr -d '\r')
method=$(printf '%s' "$req" | awk '{print $1}')
uri=$(printf '%s' "$req" | awk '{print $2}')
auth=""
clen=0
while IFS= read -r line; do
  line=$(printf '%s' "$line" | tr -d '\r')
  [ -z "$line" ] && break
  lower=$(printf '%s' "$line" | tr 'A-Z' 'a-z')
  case "$lower" in
    authorization:*) auth=${line#*: } ;;
    content-length:*) clen=$(printf '%s' "${line#*: }" | tr -d '[:space:]') ;;
  esac
done
postbody=""
if [ "${clen:-0}" -gt 0 ] 2>/dev/null; then
  postbody=$(dd bs=1 count="$clen" 2>/dev/null || true)
fi
printf '%s %s %s\n' "$method" "$uri" "$postbody" >> "$STATE/http.log"

gotb64=$(printf '%s' "$auth" | sed -n 's/.*[Bb]asic[[:space:]]*//p' | tr -d '[:space:]')
if [ "$gotb64" != "$EXPECT_B64" ]; then
  respond 401 Unauthorized '{"error":401,"message":"unauthorized"}'
  exit 0
fi

path=${uri%%\?*}
query=""
case "$uri" in *\?*) query=${uri#*\?} ;; esac
qlist=$(printf '%s' "$query" | tr '&' '\n' | sed -n 's/^list=//p')
qcomment=$(printf '%s' "$query" | tr '&' '\n' | sed -n 's/^comment=//p')

case "$method $path" in
  "GET /rest/ip/firewall/address-list")
    body='['
    first=1
    while IFS="$(printf '\t')" read -r id lst addr cmt; do
      [ -n "$id" ] || continue
      [ -n "$qlist" ] && [ "$lst" != "$qlist" ] && continue
      [ -n "$qcomment" ] && [ "$cmt" != "$qcomment" ] && continue
      [ "$first" = 1 ] || body=$body','
      first=0
      body=$body$(printf '{".id":"%s","list":"%s","address":"%s","comment":"%s"}' "$id" "$lst" "$addr" "$cmt")
    done < "$STATE/entries"
    body=$body']'
    respond 200 OK "$body"
    ;;
  "POST /rest/ip/firewall/address-list/remove")
    rid=$(printf '%s' "$postbody" | sed -n 's/.*"\.id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    tmp="$STATE/entries.new"
    : > "$tmp"
    while IFS="$(printf '\t')" read -r id lst addr cmt; do
      [ "$id" = "$rid" ] && continue
      [ -n "$id" ] || continue
      printf '%s\t%s\t%s\t%s\n' "$id" "$lst" "$addr" "$cmt" >> "$tmp"
    done < "$STATE/entries"
    mv "$tmp" "$STATE/entries"
    printf 'remove %s\n' "$rid" >> "$STATE/added.log"
    respond 200 OK '{}'
    ;;
  "POST /rest/ip/firewall/address-list/add")
    list=$(printf '%s' "$postbody" | sed -n 's/.*"list"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    addr=$(printf '%s' "$postbody" | sed -n 's/.*"address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    ttl=$(printf '%s' "$postbody" | sed -n 's/.*"timeout"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    cmt=$(printf '%s' "$postbody" | sed -n 's/.*"comment"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    nid=$(cat "$STATE/nextid")
    echo $((nid + 1)) > "$STATE/nextid"
    printf '*%s\t%s\t%s\t%s\n' "$nid" "$list" "$addr" "$cmt" >> "$STATE/entries"
    printf 'add list=%s address=%s timeout=%s comment=%s\n' "$list" "$addr" "$ttl" "$cmt" >> "$STATE/added.log"
    respond 200 OK "{\"ret\":\"*$nid\"}"
    ;;
  *)
    respond 404 NotFound '{"error":404}'
    ;;
esac
HS
chmod +x "$STATE/handler.sh"
export MOCK_ROS_STATE="$STATE"
# -e 必须在最后；-lk 保持监听。stdin/stdout 接到已接受的连接。
exec nc -lk -p "$PORT" -e "$STATE/handler.sh"
