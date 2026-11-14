# MosDNS

家庭网络 DNS。容器基于 mosdns v5.3.4，按域名规则选择国内或全局上游。出口路由不在本服务里完成，由部署侧的路由表处理。镜像为 [`jeffok/mosdns`](https://hub.docker.com/r/jeffok/mosdns)。

镜像可选同时启动 mosdns 和 BGP agent。mosdns 负责按规则选择上游；agent 为最终选中的全球应答发布和确认 BGP 路由。该功能需要 RouterOS 独立路由表和本地确认脚本，**不能仅替换 DNS 地址后视为完成部署**。联合模式和实际路由切换须先经过隔离验证。

## 解析

请求先查固定 hosts，再按 CN 例外、AI 名单、GFW 硬名单、软名单、CN 规则选择上游。AI 域名走 `DNS_AI`（留空则与 `DNS_GLOBAL` 相同），优先于 GFW 硬名单。未知域名默认 `UNKNOWN_POLICY=cn_first`：先走国内上游，接受国内段、RFC1918 和 CGNAT；其余结果回退全局上游。已知 bogus 地址丢弃。非 BGP 模式使用 mosdns 缓存；BGP 模式当前跳过 mosdns 缓存，以确保每次最终应答都经过路由确认。AAAA 默认返回空，`ENABLE_IPV6=1` 时开启。

## 规则更新

更新在容器内完成，不依赖宿主机 cron，也不依赖 RouterOS Scheduler。

入口脚本拉起 `/opt/mosdns/sync-loop.sh`。它按 `CUSTOM_HOSTS_INTERVAL`（默认 120 秒）循环：

- 域名规则在容器时区（`TZ`，默认 `Asia/Shanghai`）每天 04:30 下载。校验条目数、做金丝雀启动，内容有变化才替换并重启。失败保留当前可用规则（LKG）。
- `custom-hosts.txt` 每轮都检查。源不可用时保留现有文件。
- 镜像里的规则是构建时打入的。GitHub Actions 每周一 04:00（UTC+8）重建镜像。已经在跑的容器不等这次构建，靠上面的每日同步。

状态、历史版本和缓存 dump 在 `/data`。

## Docker Compose

```sh
mkdir -p rules data certs
cp .env.example .env
docker compose up -d
```

Compose 使用 host 网络，默认监听 `0.0.0.0:53`。主机 53 端口需要空闲，容器需要能访问配置的上游。

## 环境变量

完整示例见 [.env.example](.env.example)。规则源或 hosts 源显式留空可关闭对应同步。`DNS_AI` 留空则继承 `DNS_GLOBAL`。

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `DNS_CN` | `119.29.29.29,223.5.5.5,114.114.114.114` | 国内上游，逗号分隔 |
| `DNS_GLOBAL` | `8.8.8.8` | 全局上游，逗号分隔。szhome 为 `8.8.8.8,1.1.1.1`，这两个 /32 走隧道 |
| `DNS_AI` | 与 `DNS_GLOBAL` 相同 | AI 上游；留空则继承 `DNS_GLOBAL` |
| `UNKNOWN_POLICY` | `cn_first` | `cn_first` 或 `global_first` |
| `CONCURRENT_CN` / `CONCURRENT_GLOBAL` / `CONCURRENT_AI` | 等于对应上游个数，上限 3 | 单次查询并发数，显式值也限制在 1..3 |
| `FALLBACK_THRESHOLD` / `FALLBACK_STANDBY` | `300` ms / `false` | 未知域名双路 fallback。不要设 `true`：v5.3.4 在 primary 成功后仍可能采用 secondary 应答 |
| `ECS_PRESET` | 空 | 可选 ECS；默认不发送 |
| `ENABLE_IPV6` | `0` | `1` 时处理 AAAA |
| `CONTAINER_DNS` | `119.29.29.29` | 容器自己下载规则时用的 DNS。显式留空仍回退到该值 |
| `FETCH_RESOLVE` | 空 | `主机:端口:IP`。szhome 设 `gitea.jeffok.com:4443:192.168.88.252` |
| `RULES_SOURCES` | gh-proxy、GitHub raw、Gitea 三源 | 同一轮只用一个成功的源。显式留空关闭规则同步 |
| `CUSTOM_HOSTS_SOURCES` | Gitea 上的 `custom-hosts.txt` | 固定解析。显式留空则不拉取 |
| `CUSTOM_HOSTS_AUTH` | 空 | 可选 `Authorization` 头，不写日志 |
| `CUSTOM_HOSTS_INTERVAL` | `120` 秒 | 固定解析的检查间隔，也是同步循环的睡眠间隔 |
| `RULES_UPDATE_INTERVAL` | 备用循环 `86400` 秒 | 只在没有 `sync-loop.sh` 时生效。正常路径固定每天 04:30 |
| `RULES_PROBE_TIMEOUT` / `RULES_FETCH_TIMEOUT` | `5` / `60` 秒 | 探测与下载超时 |
| `MIN_CN`, `MIN_PROXY`, `MIN_GFW`, `MIN_AI`, `MIN_IP`, `MIN_BOGUS` | `80000`, `15000`, `3000`, `20`, `4000`, `100` | 有效条目下限 |
| `CN_EXCEPTIONS_MIN` / `APPLE_CN_MIN` | `0` / `0` | 可选文件下限，允许为空 |
| `REL_FLOOR_PCT` | `80` | 相对上一有效版本的最低比例 |
| `CACHE_SIZE` / `LAZY_CACHE_TTL` | `10240` / `120` 秒 | 非 BGP 模式的缓存容量与惰性 TTL；BGP 模式不使用 mosdns 缓存 |
| `CACHE_DUMP_INTERVAL` / `CACHE_DUMP_FILE` | `1800` 秒 / `/data/cache.dump` | 缓存 dump |
| `DNS_ENTRY_LISTEN` / `DNS_API_LISTEN` | `0.0.0.0:53` / `127.0.0.1:9091` | DNS 与管理 API |
| `RULES_DIR` / `STATE_DIR` / `MOSDNS_CONFIG` | `/etc/mosdns/rules` / `/data/state` / `/etc/mosdns/config.yaml` | 规则、状态、生成配置 |
| `DNS_ENTRY_PORT` / `EXPLAIN_DNS` | `53` / `127.0.0.1` | 运维脚本的查询地址 |
| `MOSDNS_LOG_LEVEL` / `MOSDNS_LOG_FILE` | `info` / `/etc/mosdns/mosdns.log` | 日志 |
| `HISTORY_KEEP` | `20` | 保留的规则历史份数 |
| `CANARY_PORT` / `CANARY_API` / `CANARY_SENTINEL` | `15353` / `19091` / `www.baidu.com` | 更新前金丝雀 |
| `RESTART_TIMEOUT` | `20` 秒 | 应用规则后的健康检查 |
| `DOH_ENABLED` / `DOH_CERT` / `DOH_KEY` / `DOH_LISTEN` | `0` / 默认证书路径 / 默认私钥路径 / `0.0.0.0:8443` | 可选 DoH |
| `ENABLE_ROS_BGP` / `BGP_EMBEDDED` | `0` / `0` | 启用 BGP 应答确认及同容器 agent；联合模式两者均设为 `1` |
| `ENABLE_ROS_BGP_GUARD` | `0` | 仅隔离验证镜像可启用，必须与 agent `guard_gc_enabled` 一致、路由表为 `rt-bgp-test`；生产镜像不支持，勿投入生产 |
| `BGP_LOCAL_IP` | 空 | 联合模式必填，必须与 agent 配置的 local_ip、next_hop 一致 |
| `BGP_AGENT_CONFIG` | `/etc/mosdns/bgp-agent.json` | 联合模式 agent JSON 配置路径 |
| `ROS_BGP_CONFIRM_URL` | 联合模式为 `http://127.0.0.1:9080/confirm` | 联合模式只允许此本机确认端点 |
| `ROS_BGP_CONFIRM_TIMEOUT_MS` | `6500` | mosdns 等待 agent 确认的上限；还需与 ROS DNS 和 agent 的总等待预算协调 |
| `TZ` | `Asia/Shanghai` | 容器时区，规则在本地 04:30 检查 |

## BGP 联合模式

联合镜像包含 mosdns 与 BGP agent。mosdns 保留国内、全球、AI、hosts 和 fallback 规则；全球路径实际选中的 IPv4 应答须经 agent 与 RouterOS 路由确认后才能返回。agent 和 mosdns 的某一进程退出时，入口脚本终止整个联合服务，运行时应配置经验证的自动重启策略。

1. 先准备 agent JSON 配置，字段参见 [配置示例](bgp-agent/config.example.json)；示例用于独立验证，不能直接套用到联合容器。联合模式要求 `confirm_client_ip=127.0.0.1`、`dns_listen=127.0.0.1:1053`、`http_listen=0.0.0.0:9080`，`local_ip` 和 `next_hop` 必须与 `BGP_LOCAL_IP` 一致。国内 IP 文件与 mosdns 使用同一份已校验的规则；持久状态文件放在可写挂载卷中。联合模式先设 `warm_domains=[]`，避免 agent 绕过 mosdns 最终选择直接预热并发布路由。
2. 设置 `ENABLE_ROS_BGP=1`、`BGP_EMBEDDED=1`、`BGP_LOCAL_IP`，并挂载上述 JSON 配置。当前实现跳过 mosdns 缓存，避免缓存结果缺少来源信息。
3. RouterOS 需要预先配置固定 BGP 邻居、专用 FIB 表、受限导入过滤规则及确认脚本，并限制相关管理端口访问。BGP 路由安装、备用黑洞路由、失败关闭和确认时效均须验收；详情见 [隔离验证说明](bgp-agent/ros/README.md)。
4. 先用明确限定的一台客户端验证国内和全球 DNS、缓存与首次连接的实际出口、BGP/进程重启及回退，再考虑将家庭 DNS 或路由迁移。已有客户端缓存和现存连接需要单独处理。

BGP 路由按 IP 生效，多个域名共用同一 IP 时无法为它们分别选择出口。容量上限、黑洞保留及回收、断电恢复和长连接需结合现场测试，不能仅凭 BGP 会话建立判断全家分流可用。

## 运维

```sh
docker compose logs -f mosdns
docker exec mosdns /opt/mosdns/status.sh
docker exec mosdns /opt/mosdns/fetch.sh --probe
docker exec mosdns /opt/mosdns/explain.sh example.com A
docker exec mosdns /opt/mosdns/rollback.sh --lkg
```

`status.sh` 看规则版本、LKG 和同步日志。`fetch.sh --probe` 检查规则源。`explain.sh` 看命中和应答。`rollback.sh --lkg` 回到最近一份可用规则。DoH 需要 `DOH_ENABLED=1` 并把证书放到 `certs/`。

## RouterOS 部署与验证

RouterOS 需安装与系统架构匹配的 container 包并启用容器功能，准备持久规则、状态和 agent 配置挂载。构建 ARM64 镜像并生成 RouterOS 可导入归档：

```sh
docker build --platform linux/arm64 -t jeffok/mosdns:bgp-validation .
docker save jeffok/mosdns:bgp-validation -o mosdns-oci.tar
python3 tools/ros-docker-archive.py mosdns-oci.tar mosdns-arm64.tar
```

先核对接口、容器 IP、固定 BGP 邻居和现网路由，再按 [RouterOS 隔离验证说明](bgp-agent/ros/README.md) 部署；示例脚本使用 szhome 验证地址与专用路由表，不会自动完成家庭网络切换。不要把验证服务的状态视为实际家庭分流验收。`ENABLE_IPV6=0` 是本方案当前的 IPv4 范围；客户端绕过本机 DNS 的加密 DNS、固定 IP 和共享 IP 需要另外评估。

## 目录

| 路径 | 内容 |
| --- | --- |
| `plugins/` | mosdns 的最终应答来源及 BGP 路由确认插件 |
| `scripts/` | 配置模板、入口、同步和运维脚本 |
| `rules/` | 初始 hosts 与规则；完整规则在构建时下载 |
| `tools/ros-docker-archive.py` | 把 `docker save` 转成 RouterOS 可导入的 tar |
| `bgp-agent/` | BGP 路由发布、确认、持久状态与隔离验证 |
| `tests/` | 规则同步、运行时及分流测试 |
| `data/` | 运行状态，不提交 |
| `.github/workflows/` | 冒烟测试和镜像构建 |
