# =============================================================================
# MosDNS RouterOS Container 部署参考脚本（单容器架构）
# =============================================================================
# 执行前请替换：
#   __LAN_IP__       -> RouterOS LAN IP（如 192.168.8.254）
#   __TZ__           -> Asia/Dubai（dxb）或 Asia/Shanghai（sz）
#   __SITE__         -> dxb（或 sz），与 Compose 一致，由镜像内自动选用配置，无需上传 config
#   __DOH_ENABLED__  -> 0 或 1；为 1 时需在 disk1/mosdns/certs/ 放 fullchain.pem、privkey.pem
# 前提：已执行 /system/device-mode/update container=yes 并重启
# 若拉取镜像报 SSL: no trusted CA certificate found，先执行（ROS 常见问题）：
#   /certificate/settings/set builtin-trust-anchors=trusted
# =============================================================================

# --- 步骤 1：Container 配置 ---
/container/config/set registry-url=https://registry-1.docker.io tmpdir=disk1/tmp

# --- 步骤 2：创建 veth 与桥接 ---
/interface/veth/add name=veth-mosdns address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=bridge-containers
/ip/address/add address=172.17.0.1/24 interface=bridge-containers
/interface/bridge/port add bridge=bridge-containers interface=veth-mosdns
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24

# --- 步骤 3：创建挂载与环境变量 ---
/container/mounts/add list=MOUNT_MOSDNS src=disk1/mosdns dst=/etc/mosdns

/container/envs/add list=ENV_MOSDNS key=SITE value=__SITE__
/container/envs/add list=ENV_MOSDNS key=TZ value=__TZ__
/container/envs/add list=ENV_MOSDNS key=DOH_ENABLED value=__DOH_ENABLED__
# ROS Container 不会自动注入 DNS，必须指定（Linux Docker 不需要）
/container/envs/add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8

# --- 步骤 4：添加并启动 mosdns ---
# 使用自定义镜像（entrypoint 循环：拉规则 → 启动 mosdns → 等待；crond 04:30 杀 mosdns 进程触发重载规则，不重启容器）
/container/add remote-image=jeffok/mosdns:latest interface=veth-mosdns root-dir=disk1/images/mosdns mountlists=MOUNT_MOSDNS envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes
/container/start mosdns

# --- 步骤 5：端口转发 ---
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=udp to-addresses=172.17.0.2 to-ports=53
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=tcp to-addresses=172.17.0.2 to-ports=53

# --- 步骤 6：自动拉起 ---
# 每 5 分钟执行 start（已运行则 no-op；容器崩溃时拉起）
# 每日规则更新在容器内由 crond 04:30 杀 mosdns 进程，entrypoint 循环自动重新拉规则并重启 mosdns，无需重启容器
/system/script/add name=mosdns-watchdog source={
  /container/start mosdns
}
/system/scheduler/add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog

# --- 步骤 7：排除 DNS 劫持规则 ---
# 若节点有 DSTNAT DNS 劫持规则（force lan dns），必须排除 mosdns 容器 IP，
# 否则容器的上游 DNS 查询会被 redirect 回路由器形成死循环（SERVFAIL）。
# 示例（将 __MOSDNS_IP__ 替换为 veth IP）：
# /ip/firewall/nat/set [find comment~"force lan dns udp"] src-address=!__MOSDNS_IP__
# /ip/firewall/nat/set [find comment~"force lan dns tcp"] src-address=!__MOSDNS_IP__

# =============================================================================
# 完成后：客户端 DNS 指向 __LAN_IP__
# 验证：dig @__LAN_IP__ google.com
# 若全部 SERVFAIL，检查 DNS 劫持规则是否已排除 mosdns IP（步骤 7）
# =============================================================================
