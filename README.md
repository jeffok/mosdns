# MosDNS 智能分流 DNS

基于 [MosDNS](https://github.com/IrineSistiana/mosdns) 的 CN / Global / AI 三分流，与 RouterOS `ai-sgp` address-list 联动做策略路由。单容器自更新，镜像 [jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)。

## 快速开始

**Docker Compose（szhome）**

只需设置一个站点变量和 ROS 信息，配置由镜像内根据 `SITE` 自动选用，无需复制配置文件。

```bash
# 创建 .env
echo 'SITE=sz' >> .env
echo 'ROS_HOST=192.168.88.254:6220' >> .env
echo 'ROS_PASS=你的ROS密码' >> .env

# 可选：TZ=Asia/Shanghai  DNS_SERVER=10.100.89.3

docker compose pull && docker compose up -d
```

Linux Docker Compose 使用 `network_mode: host`，DNS 由 Docker 自动注入，无需配置 `CONTAINER_DNS`。

**RouterOS Container（hkcloud / dxbhome）**

hkcloud 和 dxbhome 均使用 RouterOS 原生容器部署。部署脚本：

- **通用参考**：`scripts/routeros-setup.rsc`（替换 `__LAN_IP__`、`__TZ__`、`__SITE__`）
- **hkcloud 专用**：`scripts/hkcloud-mosdns-container.rsc`（含 DoH 配置和证书部署）

RouterOS 容器**必须**配置 `CONTAINER_DNS=8.8.8.8`，否则容器内 DNS 解析不可用（详见下方故障排查）。

## 目录与用途

| 路径 | 用途 |
|------|------|
| `configs/`、`rules/`（静态） | 打包进镜像，容器内按 `SITE` 自动选用，无需本地复制 |
| 外部规则 | entrypoint 循环内 wget 下载（每日 04:30 重载时再拉一次） |
| `updater/custom.txt` `ai-list.txt` | 自定义/AI 域名源，改后 push 触发 CI 重新生成 |
| `sites.yaml` `config.base.yaml` | 开发用源，改后 push 触发 CI 重新生成 configs |

`./certs/` 已在 docker-compose 中固定挂载；开 DoH 时在该目录放入 `fullchain.pem`、`privkey.pem` 即可。

## 环境变量（Compose）

| 变量 | 默认 | 说明 |
|------|------|------|
| SITE | sz | 站点：`hk` / `sz` / `dxb`，自动选用镜像内配置 |
| DOH_ENABLED | 0 | `1`/`true`/`yes`=开启 DoH（需放入证书文件） |
| DOH_CERT / DOH_KEY | /etc/mosdns/certs/ 下 fullchain/privkey | 仅 DoH 开启时生效，容器内路径 |
| TZ | Asia/Shanghai | 时区，crond 04:30 重载规则 |
| ROS_HOST | 空 | RouterOS SSH，`host` 或 `host:port`，空则不同步 ai-sgp |
| ROS_PASS | 空 | RouterOS admin 密码 |
| DNS_SERVER | 10.100.89.3 | 解析 AI 域名用的 DNS |
| CONTAINER_DNS | 空 | **仅 RouterOS Container**：写入容器 /etc/resolv.conf，如 `8.8.8.8` |

## 维护说明

- **规则更新**：容器内 crond 每日 04:30 杀 mosdns 进程，entrypoint 循环自动重新拉规则并启动 mosdns，**不重启容器**。
- **AI 同步**：每 2 分钟 SSH 将 `ai-list.txt` 解析出的 IP 写入 RouterOS `ai-sgp`（comment=mosdns-ai）。
- **改配置/规则**：改 `updater/*` 或 `sites.yaml`/`config.base.yaml` 后 push，CI 生成 `configs/`、`rules/`。
- **更新镜像**：Compose 执行 `docker compose pull && docker compose up -d`；RouterOS 需删容器后重新 add `jeffok/mosdns:latest`。

## CI

- **build.yml**：push main 或发布 release 时构建并推送 `jeffok/mosdns`（amd64/arm64/armv7）。
- **generate.yml**：上述源文件变更时生成 configs、rules 并提交。

## 故障排查（RouterOS Container）

**镜像拉取报 SSL 证书不受信任**：执行 `/certificate/settings/set builtin-trust-anchors=trusted`。

**容器内 wget/nslookup 报 bad address**：RouterOS Container 不会自动注入 DNS。添加环境变量 `CONTAINER_DNS=8.8.8.8`，entrypoint 会写入 `/etc/resolv.conf`。

**启动后所有查询 SERVFAIL**：检查是否存在 DNS 劫持规则（`dstnat action=redirect dst-port=53`）。mosdns 容器的 IP 必须在排除列表中，否则其上游查询会被 redirect 回路由器形成死循环。修改规则 `src-address=!<mosdns_IP>` 后清除 conntrack 并重启容器。

**规则下载全部失败（首次启动）**：RouterOS Container veth 启动后需 ~3 分钟建立出站连通性。entrypoint 已内置 `wait_for_network()` 探测（最多 180 秒），且 Dockerfile 预下载了规则文件。正常情况下首次启动即有可用规则。

**mounts/mountlists 参数报错**：mounts 使用 `name=`（非 `list=`）；部分 ROS 固件 `mountlists` 参数在 `container add` 中不可用，改为直接将文件放入 `root-dir` 对应路径。

## 许可证

见 [LICENSE](LICENSE)。
