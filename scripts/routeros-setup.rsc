# =============================================================================
# MosDNS RouterOS Container 部署参考脚本
# =============================================================================
# 本脚本仅供参考，请根据实际环境修改以下变量后再执行或分段粘贴到 RouterOS 终端。
# 执行前请确保：
#   1. 已执行 /system/device-mode/update container=yes 并重启
#   2. 已将 config.base.yaml、sites.yaml、updater/、certs/ 上传到 disk1/mosdns/
#   3. 已在 sites.yaml 中替换占位符（见 PLACEHOLDERS.md）
# =============================================================================

# ---------- 用户需修改的变量 ----------
# LAN_IP: RouterOS 的 LAN 接口 IP，用于 dstnat 端口转发（如 192.168.88.1）
# ROS_PASS: RouterOS API 密码，updater 写入 address-list 用
# SITE: 当前站点，sz / hk / sgp / dxb

# 以下占位符请在执行前替换：
#   __LAN_IP__     -> 如 192.168.88.1
#   __ROS_PASS__   -> 你的 RouterOS 密码
#   __SITE__       -> 如 sgp

# =============================================================================
# 步骤 1：Container 配置（若已完成可跳过）
# =============================================================================
/container/config/set registry-url=https://registry-1.docker.io tmpdir=disk1/tmp

# =============================================================================
# 步骤 2：创建 veth 与桥接（仅首次部署执行）
# =============================================================================
/interface/veth/add name=veth-mosdns address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=bridge-containers
/ip/address/add address=172.17.0.1/24 interface=bridge-containers
/interface/bridge/port add bridge=bridge-containers interface=veth-mosdns
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24

# =============================================================================
# 步骤 3：创建挂载与环境变量
# =============================================================================
/container/mounts/add list=MOUNT_MOSDNS src=disk1/mosdns dst=/etc/mosdns
/container/mounts/add list=MOUNT_APP src=disk1/mosdns/updater dst=/app
/container/mounts/add list=MOUNT_CERTS src=disk1/mosdns/certs dst=/etc/mosdns/certs

/container/envs/add list=ENV_MOSDNS key=SITE value=__SITE__
/container/envs/add list=ENV_MOSDNS key=MOSDNS_CONFIG_DIR value=/etc/mosdns
/container/envs/add list=ENV_MOSDNS key=RULES_DIR value=/etc/mosdns/rules
/container/envs/add list=ENV_MOSDNS key=MOSDNS_LISTEN_PORT value=53
/container/envs/add list=ENV_MOSDNS key=DOH_PORT value=8443
/container/envs/add list=ENV_MOSDNS key=DOH_CERT_DIR value=/etc/mosdns/certs
/container/envs/add list=ENV_MOSDNS key=ROS_HOST value=172.17.0.1
/container/envs/add list=ENV_MOSDNS key=ROS_PORT value=8728
/container/envs/add list=ENV_MOSDNS key=ROS_USER value=admin
/container/envs/add list=ENV_MOSDNS key=ROS_PASS value=__ROS_PASS__

# =============================================================================
# 步骤 4：添加并启动 updater 容器
# =============================================================================
/container/add remote-image=jeffok/mosdns-updater:latest interface=veth-mosdns root-dir=disk1/images/mosdns-updater mountlists=MOUNT_MOSDNS,MOUNT_APP envlist=ENV_MOSDNS name=mosdns-updater start-on-boot=yes logging=yes
/container/start mosdns-updater

# 等待 rules 生成完成（可查看 /container/print 和日志），再执行步骤 5

# =============================================================================
# 步骤 5：添加并启动 mosdns
# =============================================================================
/container/add remote-image=irinesistiana/mosdns:v5.3.3 interface=veth-mosdns root-dir=disk1/images/mosdns mountlists=MOUNT_MOSDNS,MOUNT_CERTS envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes cmd="start -c /etc/mosdns/config.yaml"
/container/start mosdns

# =============================================================================
# 步骤 6：端口转发（将 __LAN_IP__ 换成实际 LAN IP）
# DNS 标准端口 53（UDP/TCP）和 DoH 端口 8443
# =============================================================================
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=udp to-addresses=172.17.0.2 to-ports=53
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=tcp to-addresses=172.17.0.2 to-ports=53
/ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=8443 protocol=tcp to-addresses=172.17.0.2 to-ports=8443

# 若容器内使用非 53 端口（如 5353），需相应调整转发目标端口：
# /ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=udp to-addresses=172.17.0.2 to-ports=5353
# /ip/firewall/nat/add chain=dstnat dst-address=__LAN_IP__ dst-port=53 protocol=tcp to-addresses=172.17.0.2 to-ports=5353

# =============================================================================
# 步骤 7：每日规则更新与 mosdns 重启
# updater 内 crond 在 5:00 北京时间更新规则；RouterOS Scheduler 在 5:05 重启 mosdns
# =============================================================================
# 7.1 设置时区（若已为 Asia/Shanghai 可跳过）
/system/clock/set time-zone-name=Asia/Shanghai

# 7.2 创建重启脚本
/system/script/add name=mosdns-restart source={
  /container/restart mosdns
}

# 7.3 创建定时任务（每日 5:05 北京时间）
/system/scheduler/add name=mosdns-daily-restart interval=1d start-time=05:05:00 on-event=mosdns-restart

# =============================================================================
# 完成后：客户端 DNS 指向 RouterOS LAN IP（__LAN_IP__）
# 验证： dig @__LAN_IP__ google.com（标准 53 端口）
# =============================================================================
