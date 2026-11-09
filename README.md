# MosDNS

MosDNS 为家庭或小型网络提供基于域名与规则集的 DNS 解析。容器使用 mosdns v5.3.4；路由分流由部署侧路由表负责，MosDNS 不维护 RouterOS 地址列表。镜像为 [`jeffok/mosdns`](https://hub.docker.com/r/jeffok/mosdns)。

## 架构

客户端 DNS 请求进入 MosDNS 后，按顺序检查固定 hosts 与缓存，再根据 CN 例外、硬名单、软名单和 CN 规则选择国内或全局上游。未知域名默认使用 `UNKNOWN_POLICY=cn_first`：优先国内解析，接受 CN、RFC1918 与 CGNAT 地址；不接受的结果回退到全局上游。已知 bogus 地址会被丢弃。AAAA 默认返回空结果，可通过 `ENABLE_IPV6` 开启。

容器先从镜像规则或 LKG 启动服务，再由后台同步脚本更新规则。规则每天按容器本地时区在 04:30 检查；每次更新会校验内容、金丝雀启动并在变更后重启 MosDNS，失败时保留当前可用规则。`custom-hosts.txt` 每 120 秒从配置的主 URL 检查，源不可用时保留当前版本。同步状态、历史版本和缓存 dump 保存在 `/data` 挂载卷中。

## 部署

复制 `docker-compose.yml` 和 `.env.example` 到部署目录，创建 `.env`，然后运行：

```sh
mkdir -p rules data certs
docker compose up -d
```

Compose 使用 host 网络，监听地址默认为 `0.0.0.0:53`。请确保主机 53 端口可用，并按网络环境允许容器访问配置的上游 DNS。

## 环境变量

完整示例见 [.env.example](.env.example)。主要变量如下；规则源或 hosts 源显式留空可禁用对应同步，`DNS_AI` 留空则继承 `DNS_GLOBAL`。

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `DNS_CN` | `119.29.29.29,223.5.5.5,114.114.114.114` | 国内解析上游，逗号分隔 |
| `DNS_GLOBAL` | `8.8.8.8` | 全局上游；szhome 当前配置由隧道承载。`1.1.1.1` 尚未纳入 |
| `DNS_AI` | 与 `DNS_GLOBAL` 相同 | 可选 AI 专用上游 |
| `UNKNOWN_POLICY` | `cn_first` | 未知域名策略：`cn_first` 或 `global_first` |
| `CONCURRENT_CN` / `CONCURRENT_GLOBAL` / `CONCURRENT_AI` | `3` / `1` / `1` | 各上游并发数 |
| `FALLBACK_THRESHOLD` / `FALLBACK_STANDBY` | `300` ms / `true` | 未知域名双路 fallback 阈值与待命策略 |
| `ECS_PRESET` | 空 | 可选 ECS 预设地址；默认不发送 ECS |
| `ENABLE_IPV6` | `0` | `1` 开启 IPv6/AAAA 查询处理 |
| `CONTAINER_DNS` | `223.5.5.5` | 容器自身下载所用 DNS；szhome 使用此地址 |
| `FETCH_RESOLVE` | 空 | szhome 的 `223.5.5.5` 将 `gitea.jeffok.com` 解析到公网 `113.80.87.62`；szhome 可设为 `gitea.jeffok.com:4443:192.168.88.252` 固定到内网 |
| `RULES_SOURCES` | gh-proxy、GitHub raw、Gitea release-files 三源列表 | ASN-China 规则源；同一轮更新使用单一来源，显式留空可关闭规则同步 |
| `CUSTOM_HOSTS_SOURCES` | Gitea `custom-hosts.txt` URL | 固定解析文件主源；可改为自有 URL，显式留空时不拉取远端 |
| `CUSTOM_HOSTS_AUTH` | 空 | 可选 HTTP Authorization 请求头值，不会写入日志 |
| `CUSTOM_HOSTS_INTERVAL` | `120` 秒 | 固定解析文件检查间隔 |
| `RULES_UPDATE_INTERVAL` | 未设置（备用路径默认 `86400` 秒） | 仅内置备用同步循环生效；完整 `sync-loop.sh` 路径固定本地每日 04:30 |
| `RULES_PROBE_TIMEOUT` / `RULES_FETCH_TIMEOUT` | `5` / `60` 秒 | 探测与下载超时 |
| `MIN_CN`, `MIN_PROXY`, `MIN_GFW`, `MIN_AI`, `MIN_IP`, `MIN_BOGUS` | `80000`, `15000`, `3000`, `20`, `4000`, `100` | 各规则有效条目数下限 |
| `CN_EXCEPTIONS_MIN` / `APPLE_CN_MIN` | `0` / `0` | 可选规则文件下限；默认允许暂未发布的数据为空 |
| `REL_FLOOR_PCT` | `80` | 与上一有效版本相比允许的最低比例 |
| `CACHE_SIZE` / `LAZY_CACHE_TTL` | `10240` / `120` 秒 | 缓存容量与惰性缓存 TTL |
| `CACHE_DUMP_INTERVAL` / `CACHE_DUMP_FILE` | `1800` 秒 / `/data/cache.dump` | 缓存 dump 周期与文件（位于持久卷） |
| `DNS_ENTRY_LISTEN` / `DNS_API_LISTEN` | `0.0.0.0:53` / `127.0.0.1:9091` | DNS 与管理 API 监听地址 |
| `RULES_DIR` / `STATE_DIR` / `MOSDNS_CONFIG` | `/etc/mosdns/rules` / `/data/state` / `/etc/mosdns/config.yaml` | 规则、状态与生成配置路径 |
| `DNS_ENTRY_PORT` / `EXPLAIN_DNS` | `53` / `127.0.0.1` | 运维脚本查询服务地址 |
| `MOSDNS_LOG_LEVEL` / `MOSDNS_LOG_FILE` | `info` / `/etc/mosdns/mosdns.log` | 日志级别与文件 |
| `HISTORY_KEEP` | `20` | 保留的规则历史版本数 |
| `CANARY_PORT` / `CANARY_API` / `CANARY_SENTINEL` | `15353` / `19091` / `www.baidu.com` | 更新前金丝雀检查参数 |
| `RESTART_TIMEOUT` | `20` 秒 | 规则应用后的健康检查等待时间 |
| `DOH_ENABLED` / `DOH_CERT` / `DOH_KEY` / `DOH_LISTEN` | `0` / 默认证书路径 / 默认私钥路径 / `0.0.0.0:8443` | 可选 DoH 服务配置 |
| `ENABLE_ROS_SYNC` | `0` | 旧 `sync-ai` 集成默认关闭且已废弃 |
| `TZ` | `Asia/Shanghai` | 容器本地时区；规则按本地时间每日 04:30 检查 |

## 运维

```sh
docker compose logs -f mosdns
docker exec mosdns /opt/mosdns/status.sh
docker exec mosdns /opt/mosdns/fetch.sh --probe
docker exec mosdns /opt/mosdns/fetch.sh
docker exec mosdns /opt/mosdns/validate.sh
docker exec mosdns /opt/mosdns/apply.sh --now
docker exec mosdns /opt/mosdns/explain.sh example.com A
docker exec mosdns /opt/mosdns/rollback.sh --lkg
```

`status.sh` 查看当前规则、LKG、最近重启与同步日志；`fetch.sh --probe` 检查规则源可达性；`explain.sh` 检查规则命中与应答；`rollback.sh --lkg` 回退到最近可用规则。规则文件位于 `rules/`，运行状态、LKG、同步日志和历史版本位于 `data/state/`。DoH 如需启用，设置 `.env` 中 `DOH_ENABLED=1` 并将证书放入 `certs/`。`FETCH_RESOLVE` 默认留空，避免在一般部署中固定到 szhome 内网；只有 szhome 部署设置示例中的 host/port/IP。CI 的 smoke runner 基于候选镜像增加 Docker、DNS 和 HTTP 测试工具，因此测试使用与候选镜像相同的 mosdns 二进制。

## 在 RouterOS 容器中部署

> **未在 szhome 实机验证。** 下列命令按 [RouterOS 7 Container](https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container)、[Device-mode](https://help.mikrotik.com/docs/spaces/ROS/pages/93749258/Device-mode)、[DNS](https://help.mikrotik.com/docs/spaces/ROS/pages/37748767/DNS) 文档整理，目标机是 RB5009（ARM64，1GB RAM，1GB flash）。地址、接口名和 `usb1` 路径须按实机改。

### 前提

- 安装与 RouterOS 版本匹配的 extra packages 中的 `container`（架构 **arm64**），重启后 `/container/print` 可用。
- 开启容器能力并按提示用 reset 键或冷启动确认（x86 为冷启动）：

```
/system/device-mode/update container=yes
```

- **外置存储**：官方不建议把容器 `root-dir` / mounts 放在内置 flash。RB5009 内置约 1GB NAND，再叠加 RouterOS 本体后空间紧；`remote-image` 解包还要占用大量内存。请把镜像、tmpdir、rules、data 放到 USB/NVMe（下例用 `usb1`）。
- **内存**：镜像约数十 MB，本仓库实测 RSS 约 37MB。1GB RAM 够跑，但仍建议限制容器内存，并避免与其它重容器争用。

### 网络：veth、bridge、地址

与官方「bridge + NAT」示例相同，容器网段须与 LAN 不冲突：

```
/interface/veth/add name=veth-mosdns address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=containers
/ip/address/add address=172.17.0.1/24 interface=containers
/interface/bridge/port add bridge=containers interface=veth-mosdns
```

### srcnat 与放行

容器要访问规则源和 `DNS_CN` / `DNS_GLOBAL`：

```
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24 comment=mosdns-container
```

若 filter 默认会丢弃跨网段转发，在 drop 规则之前放行 LAN 访问容器 DNS（LAN 前缀按实机改）：

```
/ip firewall filter
add chain=forward action=accept src-address=192.168.88.0/24 dst-address=172.17.0.2 protocol=udp dst-port=53 comment=LAN-DNS-mosdns
add chain=forward action=accept src-address=192.168.88.0/24 dst-address=172.17.0.2 protocol=tcp dst-port=53 comment=LAN-DNS-mosdns-tcp
```

### 环境变量

按仓库 [.env.example](.env.example) 的变量逐项写入，不要把 `CUSTOM_HOSTS_AUTH` 等凭据写进可提交文件或日志。示例：

```
/container/envs
add list=ENV_MOSDNS key=TZ value="Asia/Shanghai"
add list=ENV_MOSDNS key=DNS_CN value="119.29.29.29,223.5.5.5,114.114.114.114"
add list=ENV_MOSDNS key=DNS_GLOBAL value="8.8.8.8"
add list=ENV_MOSDNS key=CONTAINER_DNS value="223.5.5.5"
add list=ENV_MOSDNS key=UNKNOWN_POLICY value="cn_first"
add list=ENV_MOSDNS key=DNS_ENTRY_LISTEN value="0.0.0.0:53"
```

其余键（`DNS_AI`、`FETCH_RESOLVE`、`RULES_SOURCES`、`CUSTOM_HOSTS_SOURCES`、阈值、缓存、DoH 等）同样 `add list=ENV_MOSDNS key=... value=...`。需要固定内网 Gitea 时再设 `FETCH_RESOLVE`，一般部署留空。

### 挂载

`src=` 是 RouterOS 路径，`dst=` 是容器内路径。rules 与 data 建议必挂；certs 仅在 `DOH_ENABLED=1` 时需要：

```
/container/mounts
add list=MOUNT_MOSDNS_RULES src=usb1/mosdns/rules dst=/etc/mosdns/rules
add list=MOUNT_MOSDNS_DATA src=usb1/mosdns/data dst=/data
add list=MOUNT_MOSDNS_CERTS src=usb1/mosdns/certs dst=/etc/mosdns/certs
```

把仓库 `rules/custom-hosts.txt` 等基线文件放到 `usb1/mosdns/rules/`，不要把 `.env` 或私钥提交进 Git。

### 添加并启动容器（ARM64）

```
/container/config/set registry-url=https://registry-1.docker.io tmpdir=usb1/tmp
```

远程拉取（需外置 tmpdir，且镜像须为 **linux/arm64**）：

```
/container/add remote-image=jeffok/mosdns:latest interface=veth-mosdns root-dir=usb1/mosdns/root \
    mountlists=MOUNT_MOSDNS_RULES,MOUNT_MOSDNS_DATA envlist=ENV_MOSDNS \
    name=mosdns start-on-boot=yes logging=yes
```

或在 ARM64 机器上 `docker save jeffok/mosdns:latest > mosdns.tar`，用 Winbox/文件拷到路由器后导入：

```
/container/add file=usb1/mosdns.tar interface=veth-mosdns root-dir=usb1/mosdns/root \
    mountlists=MOUNT_MOSDNS_RULES,MOUNT_MOSDNS_DATA envlist=ENV_MOSDNS \
    name=mosdns start-on-boot=yes logging=yes
```

启用 DoH 时把 `MOUNT_MOSDNS_CERTS` 一并写入 `mountlists`。`/container/print` 等到 `status=stopped` 后再启动：

```
/container/start mosdns
/container/set [find name=mosdns] start-on-boot=yes logging=yes
```

`logging=yes` 后可在 `/log print` 看容器输出。

### 把 RouterOS DNS 只转发到容器

先记下当前上游，供回退：

```
/ip dns print
```

关闭 DHCP 客户端写入的动态 DNS，避免 ISP 服务器盖过容器：

```
/ip dhcp-client set [find] use-peer-dns=no
/ip dns set servers=172.17.0.2
```

若 LAN 客户端继续把路由器当 DNS，再设 `allow-remote-requests=yes`，并限制仅受信网段访问 UDP/TCP 53。也可把 DHCP 的 `dns-server` 直接设为 `172.17.0.2`。

### 回退

恢复原先 `/ip dns` 上游（以及原来的 `use-peer-dns`），LAN 即可重新走旧解析；需要时再停容器：

```
/ip dns set servers=8.8.8.8,223.5.5.5
/ip dhcp-client set [find] use-peer-dns=yes
/container/stop mosdns
```

`servers=` 换成回退前记录的值。规则 LKG 回退仍用容器内 `/opt/mosdns/rollback.sh --lkg`，与 RouterOS 上游回退是两件事。

## 目录

| 路径 | 内容 |
| --- | --- |
| `scripts/` | 配置模板、入口监督、同步与运维脚本 |
| `rules/` | 初始固定解析和规则；完整规则在构建期下载 |
| `tests/` | 小夹具、mock 分流/同步验收、容器 runner 与基准 |
| `data/` | 持久状态、LKG、历史和 cache dump；不提交 |
| `.github/workflows/` | 冒烟测试及镜像构建 |
