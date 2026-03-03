# =============================================================================
# hkcloud mosdns RouterOS 容器部署（DoH，veth 10.100.50.252 已就绪）
# =============================================================================
# 前提：veth-mosdns（10.100.50.252）已创建并加入 LAN 桥
# 证书：从 10.100.50.222:/usr/local/openresty/nginx/conf/ssl 取 jeffok.com.crt、jeffok.com.key
#       在 222 上起临时 HTTP：cd /usr/local/openresty/nginx/conf/ssl && python3 -m http.server 18888
#       路由器上：/tool fetch url=http://10.100.50.222:18888/jeffok.com.crt dst-path=docker/mosdns/certs/jeffok.com.crt
#               /tool fetch url=http://10.100.50.222:18888/jeffok.com.key dst-path=docker/mosdns/certs/jeffok.com.key
# 拉取镜像前：/certificate/settings/set builtin-trust-anchors=trusted
# 拉取时 ROS DNS 需可达外网（如 8.8.8.8），完成后再设 /ip dns set servers=10.100.50.252
# =============================================================================

# --- 1. Container 配置（hkcloud 使用 docker 目录）---
/container config set registry-url=https://registry-1.docker.io tmpdir=docker/tmp

# --- 2. 挂载证书目录（name= 指定 mount 名，ROS 7.20 用 name 非 list）---
/container/mounts/add name=MC src=docker/mosdns/certs dst=/etc/mosdns/certs

# --- 3. 环境变量 ---
/container envs add list=ENV_MOSDNS key=SITE value=hk
/container envs add list=ENV_MOSDNS key=TZ value=Asia/Hong_Kong
/container envs add list=ENV_MOSDNS key=DOH_ENABLED value=1
/container envs add list=ENV_MOSDNS key=DOH_CERT value=/etc/mosdns/certs/jeffok.com.crt
/container envs add list=ENV_MOSDNS key=DOH_KEY value=/etc/mosdns/certs/jeffok.com.key
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8

# --- 4. 添加并启动（若 add 报错可去掉 mountlists=MC，改将证书 fetch 到 root-dir/etc/mosdns/certs/）---
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns root-dir=docker/images/mosdns envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes
/container start mosdns

# --- 5. DNS 与看门狗 ---
/ip dns set servers=10.100.50.252
/system script add name=mosdns-watchdog source={ /container start mosdns }
/system scheduler add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog

# =============================================================================
# 验证：dig @10.100.50.252 baidu.com；DoH https://10.100.50.252:8443/dns-query
# =============================================================================
