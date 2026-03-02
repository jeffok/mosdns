# =============================================================================
# MosDNS RouterOS Container 部署参考脚本（单容器架构）
# =============================================================================
# 执行前请替换：
#   __LAN_IP__       -> RouterOS LAN IP，如 192.168.88.254（sz）、192.168.8.254（dxb）
#   __TZ__           -> Asia/Shanghai（sz）或 Asia/Dubai（dxb）
# 前提：
#   1. 已执行 /system/device-mode/update container=yes 并重启
#   2. 已将 configs/<site>.yaml 重命名为 config.yaml 和 rules/ 上传到 disk1/mosdns/
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

/container/envs/add list=ENV_MOSDNS key=TZ value=__TZ__

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

# =============================================================================
# 完成后：客户端 DNS 指向 __LAN_IP__
# 验证：dig @__LAN_IP__ google.com
# =============================================================================
