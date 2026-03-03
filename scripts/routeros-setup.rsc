# =============================================================================
# MosDNS RouterOS Container 通用部署脚本
# =============================================================================
# 执行前请替换以下占位符：
#   __MOSDNS_IP__    -> mosdns veth IP（如 192.168.8.252 或 10.100.50.252）
#   __MOSDNS_GW__    -> veth 网关（LAN 网关 IP，如 192.168.8.254）
#   __MOSDNS_NET__   -> veth 子网（如 192.168.8.0/24）
#   __LAN_BRIDGE__   -> LAN 桥名（如 br-lan、bridge）
#   __DNS_CN__       -> CN 上游 DNS（逗号分隔，如 119.29.29.29,223.5.5.5,114.114.114.114）
#   __DNS_GLOBAL__   -> 国际上游 DNS（逗号分隔，如 1.1.1.1,8.8.8.8,9.9.9.9）
#   __TZ__           -> 时区（如 Asia/Dubai、Asia/Shanghai、Asia/Hong_Kong）
#   __DISK__         -> 存储前缀（如 disk1 或 docker）
#
# 前提：
#   /system/device-mode/update container=yes  # 重启生效
#   /certificate/settings/set builtin-trust-anchors=trusted
# =============================================================================

# --- 1. Container 配置 ---
/container/config/set registry-url=https://registry-1.docker.io tmpdir=__DISK__/tmp

# --- 2. 创建 veth 并加入 LAN 桥 ---
/interface/veth/add name=veth-mosdns address=__MOSDNS_IP__/24 gateway=__MOSDNS_GW__
/interface/bridge/port add bridge=__LAN_BRIDGE__ interface=veth-mosdns

# --- 3. 环境变量 ---
/container envs add list=ENV_MOSDNS key=DNS_CN value=__DNS_CN__
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value=__DNS_GLOBAL__
# /container envs add list=ENV_MOSDNS key=DNS_AI value=__DNS_AI__   # 空则复用 DNS_GLOBAL
/container envs add list=ENV_MOSDNS key=TZ value=__TZ__
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8
# /container envs add list=ENV_MOSDNS key=DOH_ENABLED value=1      # 启用 DoH 时取消注释
# /container envs add list=ENV_MOSDNS key=DOH_CERT value=/etc/mosdns/certs/fullchain.pem
# /container envs add list=ENV_MOSDNS key=DOH_KEY value=/etc/mosdns/certs/privkey.pem
# /container envs add list=ENV_MOSDNS key=ROS_HOST value=__MOSDNS_GW__:6220
# /container envs add list=ENV_MOSDNS key=ROS_PASS value=__PASSWORD__

# --- 4. 添加并启动容器 ---
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns \
  root-dir=__DISK__/images/mosdns envlist=ENV_MOSDNS name=mosdns \
  start-on-boot=yes logging=yes
/container start mosdns

# --- 5. DNS 指向 mosdns ---
/ip dns set servers=__MOSDNS_IP__

# --- 6. 看门狗（每 5 分钟检查，已运行则 no-op）---
/system script add name=mosdns-watchdog source={ /container start mosdns }
/system scheduler add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog

# --- 7. 排除 DNS 劫持规则 ---
# 若节点有 DSTNAT DNS 劫持规则（force lan dns），必须排除 mosdns IP，
# 否则容器的上游 DNS 查询被 redirect 回路由器形成死循环（SERVFAIL）。
# /ip/firewall/nat/set [find comment~"force lan dns udp"] src-address=!__MOSDNS_IP__
# /ip/firewall/nat/set [find comment~"force lan dns tcp"] src-address=!__MOSDNS_IP__

# =============================================================================
# 验证：dig @__MOSDNS_IP__ baidu.com && dig @__MOSDNS_IP__ google.com
# 若 SERVFAIL：检查步骤 7 DNS 劫持排除 + conntrack 清除
# =============================================================================
