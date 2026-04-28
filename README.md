# MosDNS 智能分流 DNS

基于 [mosdns](https://github.com/IrineSistiana/mosdns) 的国内/国际/AI 三路分流 DNS 服务。
镜像地址：[jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)

所有站点共用同一个镜像，不同站点的差异通过 `.env` 环境变量配置。

## Docker Compose 部署

适用于 Linux 主机或 NAS。

```bash
mkdir -p mosdns/certs mosdns/rules && cd mosdns

# 将 docker-compose.yml 和 .env.example 放入目录，然后：
cp .env.example .env
vi .env                # 按需修改 DNS 上游等参数

docker compose pull && docker compose up -d

# 测试
dig @127.0.0.1 baidu.com    # 应走国内 DNS
dig @127.0.0.1 google.com   # 应走国际 DNS
```

**关于规则文件（`rules/` 目录）：**
容器首次启动时会自动下载初始化的规则文件。如果你想在宿主机上管理（例如 `ai-list.txt`），可以在 `docker-compose.yml` 中挂载 `./rules:/etc/mosdns/rules`。容器会优先读取你挂载的文件，且不会被镜像内置文件覆盖。

如果需要 DoH，在 `certs/` 目录放入证书文件，并在 `.env` 中设置 `DOH_ENABLED=1`。

## RouterOS Container 部署

适用于 MikroTik RouterOS 7.x 设备。

**1. 开启容器功能（只需一次，执行后重启路由器）：**

```routeros
/system/device-mode/update container=yes
/certificate/settings/set builtin-trust-anchors=trusted
/container/config/set registry-url=https://registry-1.docker.io tmpdir=disk1/tmp
```

**2. 创建虚拟网卡并加入网桥：**

```routeros
/interface/veth/add name=veth-mosdns address=192.168.8.252/24 gateway=192.168.8.254
/interface/bridge/port add bridge=br-lan interface=veth-mosdns
```

> 将 IP 和网桥名 (`br-lan`) 替换为你实际的值。

**3. 设置环境变量：**

```routeros
/container envs add list=ENV_MOSDNS key=DNS_CN value=119.29.29.29,223.5.5.5
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value=1.1.1.1,8.8.8.8
/container envs add list=ENV_MOSDNS key=TZ value=Asia/Shanghai
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8
```

> `CONTAINER_DNS` 是 RouterOS 容器必须的，否则容器内部无法联网。
> 其他可选变量见下方表格。

**4. 创建并启动容器：**

```routeros
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns \
  root-dir=disk1/images/mosdns envlist=ENV_MOSDNS name=mosdns \
  start-on-boot=yes logging=yes
/container start mosdns
```

**5. 将系统 DNS 指向 mosdns：**

```routeros
/ip dns set servers=192.168.8.252
```

**6.（可选）看门狗 — 容器崩溃自动重启：**

```routeros
/system script add name=mosdns-watchdog source={ /container start mosdns }
/system scheduler add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog
```

**7.（重要）排除 DNS 劫持：**

如果你配置了 `dstnat redirect dst-port=53` 把所有 DNS 劫持到路由器，
那 mosdns 的上游查询也会被劫持回来导致死循环。
必须在 NAT 规则中把 mosdns IP 排除。

```routeros
/ip/firewall/nat/set [find comment~"force lan dns"] src-address=!192.168.8.252
```

## 环境变量

复制 `.env.example` 为 `.env`，按需修改。

| 变量 | 默认值 | 说明 |
| ------------- | ------------- | ------------- |
| **DNS_CN** | 119.29.29.29,223.5.5.5,114.114.114.114 | 国内域名上游 DNS，逗号分隔 |
| **DNS_GLOBAL** | 1.1.1.1,8.8.8.8,9.9.9.9 | 国际域名上游 DNS |
| **DNS_AI** | 同 DNS_GLOBAL | AI 域名上游 DNS（如需分流到特定网络） |
| **TZ** | Asia/Shanghai | 时区 |
| **DOH_ENABLED** | 0 | 设为 `1` 开启 DoH，需提前在 `certs/` 放好证书 |
| **DOH_CERT** | /etc/mosdns/certs/fullchain.pem | DoH 证书路径（容器内） |
| **DOH_KEY** | /etc/mosdns/certs/privkey.pem | DoH 密钥路径（容器内） |
| **ROS_HOST** | 空 | RouterOS REST API 地址，用于将 AI IP 同步到路由器地址列表 |
| **ROS_USER** | admin | RouterOS 用户名 |
| **ROS_PASS** | 空 | RouterOS 密码 |
| **AI_LIST_URL** | GitHub rules/ai-list.txt | AI 域名列表远端地址，每 2 分钟自动检查更新 |
| **RELOAD_ON_AI_LIST_CHANGE** | 1 | 当远端 AI 列表变更时，立即重载 MosDNS 让新规则生效 |
| **CONTAINER_DNS** | 空 | **RouterOS 容器必须设置**（如 8.8.8.8） |
| **DOWNLOAD_DNS** | 同 DNS_GLOBAL | 容器内部下载/更新规则时临时使用的 DNS |
| **RULE_FILE_MAX_AGE** | 82800 (23小时) | 规则文件过期时间（秒），设为 `0` 则每次强制下载 |
| **RELOAD_DELAY** | 0 | 每日 04:30 规则重载前的延迟秒数，用于多站点错开时间 |

## 运行逻辑说明

*   **规则自动更新**：容器每天 04:30 触发基础规则重载，超过 `RULE_FILE_MAX_AGE` 的文件会自动联网更新。
*   **AI 列表同步**：脚本每 2 分钟检查 `AI_LIST_URL`，如有变化会自动重载 MosDNS 且无需重启容器。
*   **本地挂载保护**：容器启动时，仅当 `/etc/mosdns/rules` 目录下缺失规则文件时，才会从镜像内置模板自动补齐。已存在的自定义文件不会被覆盖。
*   **更新镜像**：Compose 执行 `docker compose pull && docker compose up -d`；ROS 则需停止旧容器并重新创建。

## 常见问题

**1. 容器内下载规则文件报错（如 Connection refused 或 timeout）**
→ 容器内部 DNS 无法解析 GitHub。请设置环境变量 `DOWNLOAD_DNS=8.8.8.8`，脚本会在下载时自动切换该 DNS。

**2. 容器启动后所有查询返回 SERVFAIL**
→ 路由器开启了 DNS 劫持但没有排除 MosDNS IP，形成死循环。请参考上方第 7 步修复劫持规则。

**3. 拉取镜像失败 / SSL 错误**
→ 执行 `/certificate/settings/set builtin-trust-anchors=trusted` 并重启设备。

**4. 在 ROS 中使用 `mounts` 报错**
→ RouterOS 7.20 版本的 mounts 语法应使用 `name=`。如果不兼容，可直接将规则文件复制到容器的 `root-dir` 对应目录下。
