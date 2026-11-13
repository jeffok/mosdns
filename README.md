# MosDNS

家庭网络 DNS。容器基于 mosdns v5.3.4，按域名规则选择国内或全局上游。出口路由不在本服务里完成，由部署侧的路由表处理。镜像为 [`jeffok/mosdns`](https://hub.docker.com/r/jeffok/mosdns)。

RouterOS 有两种可选集成，默认都关闭，互不影响：

- `ENABLE_ROS_ADDRLIST`：按最终应答写入地址池。szhome 使用这种方式。
- `ENABLE_ROS_SYNC`：按 `ai-domains.txt` 周期重写地址池。szhome 不使用。

## 解析

请求先查固定 hosts 和缓存，再按 CN 例外、AI 名单、GFW 硬名单、软名单、CN 规则选择上游。AI 域名走 `DNS_AI`（留空则与 `DNS_GLOBAL` 相同），优先于 GFW 硬名单。未知域名默认 `UNKNOWN_POLICY=cn_first`：先走国内上游，接受国内段、RFC1918 和 CGNAT；其余结果回退全局上游。已知 bogus 地址丢弃。AAAA 默认返回空，`ENABLE_IPV6=1` 时开启。

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
| `CACHE_SIZE` / `LAZY_CACHE_TTL` | `10240` / `120` 秒 | 缓存容量与惰性 TTL |
| `CACHE_DUMP_INTERVAL` / `CACHE_DUMP_FILE` | `1800` 秒 / `/data/cache.dump` | 缓存 dump |
| `DNS_ENTRY_LISTEN` / `DNS_API_LISTEN` | `0.0.0.0:53` / `127.0.0.1:9091` | DNS 与管理 API |
| `RULES_DIR` / `STATE_DIR` / `MOSDNS_CONFIG` | `/etc/mosdns/rules` / `/data/state` / `/etc/mosdns/config.yaml` | 规则、状态、生成配置 |
| `DNS_ENTRY_PORT` / `EXPLAIN_DNS` | `53` / `127.0.0.1` | 运维脚本的查询地址 |
| `MOSDNS_LOG_LEVEL` / `MOSDNS_LOG_FILE` | `info` / `/etc/mosdns/mosdns.log` | 日志 |
| `HISTORY_KEEP` | `20` | 保留的规则历史份数 |
| `CANARY_PORT` / `CANARY_API` / `CANARY_SENTINEL` | `15353` / `19091` / `www.baidu.com` | 更新前金丝雀 |
| `RESTART_TIMEOUT` | `20` 秒 | 应用规则后的健康检查 |
| `DOH_ENABLED` / `DOH_CERT` / `DOH_KEY` / `DOH_LISTEN` | `0` / 默认证书路径 / 默认私钥路径 / `0.0.0.0:8443` | 可选 DoH |
| `ENABLE_ROS_ADDRLIST` | `0` | `1` 时把全局/AI 路径的非国内 IPv4 写入地址池。缺 URL 或密码则关闭插件，DNS 继续 |
| `ROS_ADDRLIST_URL` / `ROS_USER` / `ROS_PASS` | 空 / `admin` / 空 | REST 地址、用户、密码。只用 HTTP。密码只来自环境变量 |
| `ROS_ADDRLIST_LIST` | `gfw-proxy` | 地址池名称 |
| `ROS_ADDRLIST_TIMEOUT` | `5m` | 动态条目租约，应长于下游 DNS 缓存 |
| `ROS_ADDRLIST_TTL_MAX` | `300` | 全局/AI 应答 TTL 上限（秒） |
| `ROS_ADDRLIST_SYNC_BUDGET_MS` | `80` | 新地址同步写入的等待上限，超时先应答 |
| `TZ` | `Asia/Shanghai` | 容器时区，规则在本地 04:30 检查 |

## 动态地址池

`ENABLE_ROS_ADDRLIST=1` 后，入口先完成原有解析（含 fallback 和缓存），再检查最终应答里的 IPv4。

`ros_tag` 把全局/AI 路径的公网 IPv4 记入内存来源表。`ros_confirm` 对最终应答逐个判断：没有来源记录、属于国内段、私网或 CGNAT 的地址不写。comment 不是 `mosdns-dyn` 的静态条目不覆盖。剩余租约大于一半则跳过；不足一半则后台续约；没有条目或已过期则同步 `PUT`，等待上限默认 80ms，超时先返回 DNS 应答。来源表可落在 `/data/state/ros_source.json`。

`ros_tag` 和 `ros_confirm` 必须定义在引用它们的 sequence 之前，否则启动失败。

RouterOS 账号需要 `read,write,api,rest-api`。只有 `rest-api`、没有 `api` 时 HTTP 500。`write` 不能限制到某一个地址池。传输只用 HTTP。

内存里的租约跟进程走。只在 RouterOS 上删除动态条目，进程不会立刻补写，要等租约进入后半段，或重启容器。

## 按名单周期同步

`ENABLE_ROS_SYNC=1` 时，后台按 `SYNC_AI_INTERVAL`（默认 120 秒）读取正在使用的 `ai-domains.txt`，把解析出的 IPv4 写入地址池（默认 `ai-sgp`）。它不下载规则，也不重载 mosdns。关闭时相关变量留空即可，不会启动、不会告警。

szhome 的分流不走这套同步。需要独立 AI 出口的其它站点才打开它。

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `ROS_HOST` | 空 | RouterOS 地址，可带端口 |
| `ROS_USER` / `ROS_PASS` | `admin` / 空 | REST 账号。日志不打印密码 |
| `ROS_SCHEME` | `http` | `http` 或 `https` |
| `ROS_TLS_INSECURE` | `0` | 自签证书时设 `1` |
| `ROS_AI_LIST` | `ai-sgp` | 地址池 |
| `ROS_AI_TTL` | `1800s` | 条目超时 |
| `ROS_AI_COMMENT` | `mosdns-ai` | 用于识别本脚本写入的条目 |
| `SYNC_AI_DNS` | 与 `CONTAINER_DNS` 相同 | 建议 `127.0.0.1`，使写入的地址和客户端看到的一致 |

每轮先按 comment 删除旧条目再写入。空窗期内的新连接会走默认路由，已建立的连接不受影响。

## 运维

```sh
docker compose logs -f mosdns
docker exec mosdns /opt/mosdns/status.sh
docker exec mosdns /opt/mosdns/fetch.sh --probe
docker exec mosdns /opt/mosdns/explain.sh example.com A
docker exec mosdns /opt/mosdns/rollback.sh --lkg
```

`status.sh` 看规则版本、LKG 和同步日志。`fetch.sh --probe` 检查规则源。`explain.sh` 看命中和应答。`rollback.sh --lkg` 回到最近一份可用规则。DoH 需要 `DOH_ENABLED=1` 并把证书放到 `certs/`。

## RouterOS

通用写法用独立容器网段。szhome 把容器放在 LAN 上，不另建 bridge 和 NAT。官方说明见 [Container](https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container)、[Device-mode](https://help.mikrotik.com/docs/spaces/ROS/pages/93749258/Device-mode)。

### 通用

安装与系统匹配的 `container` 包（ARM64 设备用 arm64），然后：

```
/system/device-mode/update container=yes
```

按提示复位或冷启动。官方不建议把 `root-dir` 放在内置 flash；有 USB 或 NVMe 时用外置盘。镜像解包需要额外内存，建议设置 `memory-high`。

```
/interface/veth/add name=veth-mosdns address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=containers
/ip/address/add address=172.17.0.1/24 interface=containers
/interface/bridge/port add bridge=containers interface=veth-mosdns
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24
```

环境变量写入 `ENV_MOSDNS`，挂载 rules 与 data，再 `remote-image=jeffok/mosdns:latest` 或导入下面的 tar。RouterOS DNS 指到 `172.17.0.2`，并关掉 DHCP 客户端的 `use-peer-dns`。若客户端仍使用路由器地址，打开 `allow-remote-requests`，并只允许受信网段访问 53。

`docker save` 的原样 tar，RouterOS 会报 `no config found in manifest`。在 ARM64 上构建后转换：

```sh
docker build --platform linux/arm64 -t jeffok/mosdns:ros-addrlist .
docker save jeffok/mosdns:ros-addrlist -o mosdns-oci.tar
python3 tools/ros-docker-archive.py mosdns-oci.tar mosdns-arm64.tar
```

### szhome

RB5009UG+S+，RouterOS 7.24.6，无外置盘。容器与 LAN 同网段。

| 项目 | 值 |
| --- | --- |
| 容器地址 | `192.168.88.222/24`，`veth-mosdns` 接入 `br-lan`，网关 `192.168.88.254` |
| 目录 | `root-dir=mosdns/root-ros`，规则 `/mosdns/rules`，状态 `/mosdns/data` |
| 环境变量 | 只用 `ENV_MOSDNS` |
| 镜像 | 本仓库 ARM64 构建，标签 `jeffok/mosdns:ros-addrlist`，经 `tools/ros-docker-archive.py` 导入 |
| 资源 | `memory-high=200MiB`，`start-on-boot=yes`，`logging=yes` |

```
/container envs add list=ENV_MOSDNS key=TZ value="Asia/Shanghai"
/container envs add list=ENV_MOSDNS key=DNS_ENTRY_LISTEN value="0.0.0.0:53"
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value="8.8.8.8,1.1.1.1"
/container envs add list=ENV_MOSDNS key=FETCH_RESOLVE value="gitea.jeffok.com:4443:192.168.88.252"
/container envs add list=ENV_MOSDNS key=ENABLE_ROS_ADDRLIST value="1"
/container envs add list=ENV_MOSDNS key=ROS_ADDRLIST_URL value="http://192.168.88.254"
/container envs add list=ENV_MOSDNS key=ROS_USER value="mosdns"
/container envs add list=ENV_MOSDNS key=ROS_PASS value="<密码>"
/container envs add list=ENV_MOSDNS key=ROS_ADDRLIST_TIMEOUT value="5m"
```

`ROS_PASS` 只放在路由器的环境变量里。`/container envs print` 和容器日志会显示它，不要把输出提交到仓库。

写入账号 `mosdns`，来源限制 `192.168.88.222/32`。策略必须包含 `read,write,api,rest-api`，并且不要给 `local`、`ssh`、`telnet`、`winbox`、`web`：

```
/user group add name=mosdns-sync policy=read,write,api,rest-api,!local,!telnet,!ssh,!ftp,!reboot,!policy,!test,!winbox,!password,!web,!sniff,!sensitive,!romon
/user add name=mosdns group=mosdns-sync address=192.168.88.222/32 password=<密码>
```

路由器 DNS：

```
/ip dns set servers=192.168.88.222 cache-max-ttl=2m allow-remote-requests=yes
```

DHCP 的 `dns-server` 仍是 `192.168.88.254`，由路由器转发到容器。`servers` 不要再加 `119.29.29.29`，否则一部分查询不经过 mosdns，地址不会进地址池。`cache-max-ttl=2m` 短于地址池租约 5 分钟。

分流：

| 流量 | 出口 |
| --- | --- |
| 国内路径，以及没有写入 `gfw-proxy` 的地址 | 默认路由 `pppoe-cn` |
| 全局/AI 路径的非国内 IPv4 | `gfw-proxy`，comment 前缀 `mosdns-dyn`，租约 5 分钟；`br-lan` 新连接标记 `cm-ovpn`，走 `rt-ovpn` → `ovpn-szof` |
| `gitea` / `reg` / `gitlab.jeffok.com` | `192.168.88.252`，来自 `custom-hosts.txt` |

`8.8.8.8/32` 和 `1.1.1.1/32` 走 `ovpn-szof`，供 `DNS_GLOBAL` 使用。隧道端点 `183.56.180.152/32` 走 `pppoe-cn`。`rt-ovpn` 的默认路由是 `ovpn-szof`。地址池保留占位 `255.255.255.255`（comment `gfw-proxy-keep`）。

mangle 只匹配从 `br-lan` 进入的新连接，路由器自己发出的包不打标。命中地址池的 UDP/443 与 TCP 走同一条连接标记。隧道是 TCP 8881。

路由器 `disable-ipv6=yes`。容器保持 `ENABLE_IPV6=0`。

不导入 `comment=CN` 的明细路由，也不运行 `Update_CN_Routes`。国内流量走默认的电信路由。

验证：

```
/ip dns cache flush
:put [:resolve www.baidu.com server=192.168.88.222]
:put [:resolve www.google.com server=192.168.88.222]
```

国内名不出现在 `gfw-proxy`。`www.google.com` 的地址以 `mosdns-dyn` 写入，租约约 5 分钟。清缓存后再解析，否则看到的是路由器缓存，不会触发写入。

DoH、加密 DNS 或 Private Relay 不经过这台 mosdns，对应地址不会进地址池。

停止分流时去掉 `cm-ovpn` 两条 mangle，或把 `/ip dns servers` 改回国内递归（例如 `119.29.29.29`）。规则本身回退用容器里的 `rollback.sh --lkg`。

## 目录

| 路径 | 内容 |
| --- | --- |
| `plugins/ros_addrlist/` | 按应答写入 RouterOS 地址池 |
| `scripts/` | 配置模板、入口、同步和运维脚本 |
| `rules/` | 初始 hosts 与规则；完整规则在构建时下载 |
| `tools/ros-docker-archive.py` | 把 `docker save` 转成 RouterOS 可导入的 tar |
| `tests/` | 同步、运行时和地址池测试 |
| `data/` | 运行状态，不提交 |
| `.github/workflows/` | 冒烟测试和镜像构建 |
