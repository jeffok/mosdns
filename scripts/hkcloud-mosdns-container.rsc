# =============================================================================
# hkcloud mosdns RouterOS 容器部署（DoH，veth 10.100.50.252 已就绪）
# =============================================================================
# 前提：veth-mosdns（10.100.50.252）已创建并加入 LAN 桥
# 证书：从 10.100.50.222 取 jeffok.com.crt、jeffok.com.key
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
/container envs add list=ENV_MOSDNS key=DNS_CN value=119.29.29.29,223.5.5.5,114.114.114.114
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value=1.1.1.1,8.8.8.8,9.9.9.9
/container envs add list=ENV_MOSDNS key=DNS_AI value=10.100.89.3
/container envs add list=ENV_MOSDNS key=TZ value=Asia/Hong_Kong
/container envs add list=ENV_MOSDNS key=DOH_ENABLED value=1
/container envs add list=ENV_MOSDNS key=DOH_CERT value=/etc/mosdns/certs/jeffok.com.crt
/container envs add list=ENV_MOSDNS key=DOH_KEY value=/etc/mosdns/certs/jeffok.com.key
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8
/container envs add list=ENV_MOSDNS key=RELOAD_DELAY value=20
/container envs add list=ENV_MOSDNS key=ROS_HOST value=10.100.50.254:6220
/container envs add list=ENV_MOSDNS key=ROS_PASS value=Wangke.0912

# --- 4. 添加并启动 ---
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns root-dir=docker/images/mosdns envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes
/container start mosdns

# --- 5. DNS 与看门狗 ---
/ip dns set servers=10.100.50.252
/system script add name=mosdns-watchdog source={ /container start mosdns }
/system scheduler add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog

# --- 6. 排除 DNS 劫持规则（关键！否则容器上游查询被 redirect 回路由器导致 SERVFAIL）---
/ip firewall nat set [find comment="[hkcloud] force lan dns udp"] src-address=!10.100.50.252
/ip firewall nat set [find comment="[hkcloud] force lan dns tcp"] src-address=!10.100.50.252

# =============================================================================
# 验证：dig @10.100.50.252 baidu.com；DoH https://10.100.50.252:8443/dns-query
# 若 SERVFAIL，检查：1) DNS 劫持规则是否排除了 .252  2) conntrack 是否有残留（清除后重启容器）
# =============================================================================
