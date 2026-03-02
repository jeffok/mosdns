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
# 使用自定义镜像（含 entrypoint.sh 自动下载规则 + crond 定时重启）
/container/add remote-image=jeffok/mosdns:latest interface=veth-mosdns root-dir=disk1/images/mosdns mountlists=MOUNT_MOSDNS envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes
/container/start mosdns

# --- 步骤 5：端口转发 ---
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=udp to-addresses=172.17.0.2 to-ports=53
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=tcp to-addresses=172.17.0.2 to-ports=53

# --- 步骤 6：每日重启 ---
# entrypoint.sh 中 crond 在 04:30 执行 kill PID1 使容器停止
# RouterOS scheduler 在 04:35 重新启动容器（触发 entrypoint 重新下载规则）

/system/clock/set time-zone-name=__TZ__

/system/script/add name=mosdns-restart source={
  /container/start mosdns
}

/system/scheduler/add name=mosdns-daily-restart interval=1d start-time=04:35:00 on-event=mosdns-restart

# =============================================================================
# 完成后：客户端 DNS 指向 __LAN_IP__
# 验证：dig @__LAN_IP__ google.com
# =============================================================================
