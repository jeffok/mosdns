# MosDNS

MosDNS 为家庭或小型网络提供基于域名与规则集的 DNS 解析。容器使用 mosdns v5.3.4；路由分流由部署侧路由表负责，MosDNS 不维护 RouterOS 地址列表。镜像为 [`jeffok/mosdns`](https://hub.docker.com/r/jeffok/mosdns)。

## 架构

客户端 DNS 请求进入 MosDNS 后，按顺序检查固定 hosts 与缓存，再根据 CN 例外、硬名单、软名单和 CN 规则选择国内或全局上游。未知域名默认使用 `UNKNOWN_POLICY=cn_first`：优先国内解析，接受 CN、RFC1918 与 CGNAT 地址；不接受的结果回退到全局上游。已知 bogus 地址会被丢弃。AAAA 默认返回空结果，可通过 `ENABLE_IPV6` 开启。

容器先从镜像规则或 LKG 启动服务，再由后台同步脚本更新规则。规则更新会校验内容、金丝雀启动并在变更后重启 MosDNS；失败时保留当前可用规则。`custom-hosts.txt` 定期从配置的主 URL 拉取，源不可用时保留当前版本。同步状态与历史版本保存在 `/data/state`。

## 部署

复制 `docker-compose.yml` 和 `.env.example` 到部署目录，创建 `.env`，然后运行：

```sh
mkdir -p rules data certs
docker compose up -d
```

Compose 使用 host 网络，监听地址默认为 `0.0.0.0:53`。请确保主机 53 端口可用，并按网络环境允许容器访问配置的上游 DNS。

## 环境变量

完整示例见 [.env.example](.env.example)。主要变量如下；空值表示使用脚本内的默认值。

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `DNS_CN` | `119.29.29.29,223.5.5.5,114.114.114.114` | 国内解析上游，逗号分隔 |
| `DNS_GLOBAL` | `8.8.8.8` | 全局上游；szhome 当前配置由隧道承载。`1.1.1.1` 尚未纳入 |
| `DNS_AI` | 与 `DNS_GLOBAL` 相同 | 可选 AI 专用上游 |
| `UNKNOWN_POLICY` | `cn_first` | 未知域名策略：`cn_first` 或 `global_first` |
| `ECS_PRESET` | 空 | 可选 ECS 预设地址；默认不发送 ECS |
| `ENABLE_IPV6` | `0` | 开启后保留 AAAA 查询处理 |
| `CONTAINER_DNS` | `223.5.5.5` | 容器自身下载所用 DNS；szhome 使用此地址 |
| `FETCH_RESOLVE` | `gitea.jeffok.com:4443:192.168.88.252` | 可选固定下载源解析，格式为 `host:port:ip` |
| `RULES_SOURCES` | `.env.example` 中的有序列表 | ASN-China 规则源；同一轮更新使用单一来源 |
| `CUSTOM_HOSTS_SOURCES` | szhome Gitea `custom-hosts.txt` URL | 固定解析文件的主源；可改为自有主源 URL |
| `CUSTOM_HOSTS_AUTH` | 空 | 可选 HTTP Authorization 请求头值 |
| `RULES_UPDATE_INTERVAL` | `86400` 秒 | 规则更新间隔 |
| `CUSTOM_HOSTS_INTERVAL` | `120` 秒 | 固定解析文件检查间隔 |
| `RULES_PROBE_TIMEOUT` / `RULES_FETCH_TIMEOUT` | `5` / `60` 秒 | 探测与下载超时 |
| `MIN_CN`, `MIN_PROXY`, `MIN_GFW`, `MIN_AI`, `MIN_IP`, `MIN_BOGUS` | `80000`, `15000`, `3000`, `20`, `4000`, `100` | 各规则有效条目数下限 |
| `CN_EXCEPTIONS_MIN` / `APPLE_CN_MIN` | `0` / `0` | 可选规则文件下限；默认允许暂未发布的数据为空 |
| `REL_FLOOR_PCT` | `80` | 与上一有效版本相比允许的最低比例 |
| `CACHE_SIZE` / `LAZY_CACHE_TTL` | `10240` / `120` 秒 | 缓存容量与惰性缓存 TTL |
| `CACHE_DUMP_INTERVAL` | `1800` 秒 | 缓存 dump 周期 |
| `DNS_ENTRY_LISTEN` / `DNS_API_LISTEN` | `0.0.0.0:53` / `127.0.0.1:9091` | DNS 与管理 API 监听地址 |
| `MOSDNS_LOG_LEVEL` / `MOSDNS_LOG_FILE` | `info` / `/etc/mosdns/mosdns.log` | 日志级别与文件 |
| `ENABLE_ROS_SYNC` | `0` | 旧 `sync-ai` 集成默认关闭且已废弃 |
| `TZ` | `Asia/Shanghai` | 容器时区 |

## 运维

```sh
docker compose logs -f mosdns
docker exec mosdns /opt/mosdns/status.sh
docker exec mosdns /opt/mosdns/fetch.sh --probe
docker exec mosdns /opt/mosdns/explain.sh example.com A
docker exec mosdns /opt/mosdns/rollback.sh --lkg
```

`status.sh` 查看当前规则、LKG、最近重启与同步日志；`fetch.sh --probe` 检查规则源可达性；`explain.sh` 检查规则命中与应答；`rollback.sh --lkg` 回退到最近可用规则。规则文件位于 `rules/`，运行状态、LKG、同步日志和历史版本位于 `data/state/`。DoH 如需启用，设置 `.env` 中 `DOH_ENABLED=1` 并将证书放入 `certs/`。
