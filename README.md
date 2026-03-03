# MosDNS 智能分流 DNS

基于 [MosDNS](https://github.com/IrineSistiana/mosdns) 的 CN / Global / AI 三分流，与 RouterOS `ai-sgp` address-list 联动做策略路由。通用镜像 [jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)，通过环境变量适配任意站点。

## 快速开始

所有站点使用同一个镜像，差异全部通过 `.env` 环境变量控制。

### Docker Compose（Linux 主机）

```bash
mkdir -p mosdns/certs && cd mosdns

# 1. 复制 docker-compose.yml（或从仓库获取）
# 2. 复制 .env.example 为 .env，按站点修改
cp .env.example .env
vi .env

# 3. 启动
docker compose pull && docker compose up -d

# 4. 验证
dig @<本机IP> baidu.com
dig @<本机IP> google.com
```

### RouterOS Container

```routeros
# 前提
/system/device-mode/update container=yes   # 重启生效
/certificate/settings/set builtin-trust-anchors=trusted

# 设置环境变量（按站点修改值）
/container envs add list=ENV_MOSDNS key=DNS_CN value=119.29.29.29,223.5.5.5,114.114.114.114
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value=1.1.1.1,8.8.8.8,9.9.9.9
/container envs add list=ENV_MOSDNS key=TZ value=Asia/Shanghai
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8

# 拉取并启动
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns \
  root-dir=disk1/images/mosdns envlist=ENV_MOSDNS name=mosdns \
  start-on-boot=yes logging=yes
/container start mosdns
```

详见 `scripts/routeros-setup.rsc`（通用）和 `scripts/hkcloud-mosdns-container.rsc`（hkcloud DoH 专用）。

## 环境变量

复制 `.env.example` 为 `.env`，按站点需求修改。

| 变量 | 默认 | 说明 |
|------|------|------|
| DNS_CN | 119.29.29.29,223.5.5.5,114.114.114.114 | CN 上游 DNS（逗号分隔，支持 IP:PORT） |
| DNS_GLOBAL | 1.1.1.1,8.8.8.8,9.9.9.9 | 国际上游 DNS |
| DNS_AI | （复用 DNS_GLOBAL） | AI 上游 DNS |
| DOH_ENABLED | 0 | `1`=开启 DoH（需在 `./certs/` 放证书） |
| DOH_CERT / DOH_KEY | /etc/mosdns/certs/ 下 fullchain/privkey | DoH 证书容器内路径 |
| TZ | Asia/Shanghai | 时区，影响 crond 04:30 规则重载 |
| ROS_HOST | 空 | RouterOS SSH（`host:port`），空则不同步 ai-sgp |
| ROS_PASS | 空 | RouterOS admin 密码 |
| DNS_SERVER | （取 DNS_GLOBAL 第一个） | sync-ai 解析用 DNS |
| RELOAD_DELAY | 0 | 04:30 重载前延迟秒数，多站点错开用 |
| CONTAINER_DNS | 空 | **仅 ROS Container**：写入 /etc/resolv.conf |

## 各站点 .env 示例

**szhome**（Docker Compose，192.168.88.252）：
```
DNS_CN=119.29.29.29,223.5.5.5,114.114.114.114
DNS_GLOBAL=10.100.50.252
TZ=Asia/Shanghai
ROS_HOST=192.168.88.254:6220
ROS_PASS=你的密码
RELOAD_DELAY=40
```

**hkcloud**（ROS 容器，10.100.50.252，启用 DoH）：
```
DNS_CN=119.29.29.29,223.5.5.5,114.114.114.114
DNS_GLOBAL=1.1.1.1,8.8.8.8,9.9.9.9
DNS_AI=10.100.89.3
DOH_ENABLED=1
DOH_CERT=/etc/mosdns/certs/jeffok.com.crt
DOH_KEY=/etc/mosdns/certs/jeffok.com.key
TZ=Asia/Hong_Kong
CONTAINER_DNS=8.8.8.8
ROS_HOST=10.100.50.254:6220
ROS_PASS=你的密码
RELOAD_DELAY=20
```

**dxbhome**（ROS 容器，192.168.8.252）：
```
DNS_CN=10.100.50.252
DNS_GLOBAL=1.1.1.1,8.8.8.8,9.9.9.9
TZ=Asia/Dubai
CONTAINER_DNS=8.8.8.8
ROS_HOST=192.168.8.254:6220
ROS_PASS=你的密码
RELOAD_DELAY=60
```

## 维护说明

- **规则更新**：容器内 crond 每日 04:30 杀 mosdns 进程，entrypoint 自动重新拉规则并启动，不重启容器
- **AI 同步**：每 2 分钟 SSH 将 `ai-list.txt` 解析出的 IP 写入 RouterOS `ai-sgp`
- **更新镜像**：Compose 执行 `docker compose pull && docker compose up -d`；RouterOS 需 `stop` → `remove` → `add` 重建容器

## 故障排查（RouterOS Container）

**镜像拉取报 SSL 证书不受信任**：执行 `/certificate/settings/set builtin-trust-anchors=trusted`

**容器内 wget/nslookup 报 bad address**：添加环境变量 `CONTAINER_DNS=8.8.8.8`

**启动后所有查询 SERVFAIL**：检查是否存在 DNS 劫持规则（`dstnat action=redirect dst-port=53`），mosdns 容器 IP 必须在排除列表中。修改 `src-address=!<mosdns_IP>` 后清除 conntrack 并重启容器

**规则下载全部失败（首次启动）**：ROS Container veth 启动后需 ~3 分钟建立出站连通性，entrypoint 已内置等待探测（最多 180 秒），且镜像预下载了规则文件

**mounts/mountlists 参数报错**：mounts 使用 `name=`（非 `list=`）；部分固件 `mountlists` 不可用，改为将文件放入 `root-dir` 对应路径

## 许可证

见 [LICENSE](LICENSE)。
