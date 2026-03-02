# MosDNS 智能分流 DNS

基于 [MosDNS](https://github.com/IrineSistiana/mosdns) 的 CN / Global / AI 三分流，与 RouterOS `ai-sgp` address-list 联动做策略路由。单容器自更新，镜像 [jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)。

## 快速开始

**Docker Compose（hkcloud / szhome）**

只需设置一个站点变量和 ROS 信息，配置由镜像内根据 `SITE` 自动选用，无需复制配置文件。

```bash
# 创建 .env（按站点选一个）
echo 'SITE=hk' >> .env
echo 'ROS_HOST=10.100.50.254:6220' >> .env
echo 'ROS_PASS=你的ROS密码' >> .env

# 可选：TZ=Asia/Shanghai  DNS_SERVER=10.100.89.3

docker compose pull && docker compose up -d
```

- **hkcloud**：`SITE=hk`，`ROS_HOST` 填 hkcloud 上 RouterOS。开 DoH 时 `DOH_ENABLED=1`，并在 `./certs/` 放入证书。
- **szhome**：`SITE=sz`，`ROS_HOST` 填 szhome 的 RouterOS；任一站点均可 `DOH_ENABLED=1` 开启 DoH（本机证书）。

**RouterOS Container（dxbhome）**：按 `scripts/routeros-setup.rsc` 执行，替换 `__LAN_IP__`、`__TZ__`、`__SITE__=dxb`；配置由镜像按 SITE 自动选用，无需上传 config。开 DoH 时在 disk1/mosdns/certs/ 放证书并增加 env `DOH_ENABLED=1`。

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
| DOH_ENABLED | 0 | `1`/`true`/`yes`=开启 DoH（本机证书，需在 `./certs/` 放 fullchain.pem、privkey.pem） |
| DOH_CERT / DOH_KEY | ./certs 下 fullchain/privkey | 仅 DoH 开启时生效，默认挂载路径 |
| TZ | Asia/Shanghai | 时区，crond 04:30 重载规则 |
| ROS_HOST | 空 | RouterOS SSH，`host` 或 `host:port`，空则不同步 ai-sgp |
| ROS_PASS | 空 | RouterOS admin 密码 |
| DNS_SERVER | 10.100.89.3 | 解析 AI 域名用的 DNS |

## 维护说明

- **规则更新**：容器内 crond 每日 04:30 杀 mosdns 进程，entrypoint 循环自动重新拉规则并启动 mosdns，**不重启容器**。
- **AI 同步**：每 2 分钟 SSH 将 `ai-list.txt` 解析出的 IP 写入 RouterOS `ai-sgp`（comment=mosdns-ai）。
- **改配置/规则**：改 `updater/*` 或 `sites.yaml`/`config.base.yaml` 后 push，CI 生成 `configs/`、`rules/`。
- **更新镜像**：Compose 执行 `docker compose pull && docker compose up -d`；RouterOS 需删容器后重新 add `jeffok/mosdns:latest`。

## CI

- **build.yml**：push main 或发布 release 时构建并推送 `jeffok/mosdns`（amd64/arm64/armv7）。
- **generate.yml**：上述源文件变更时生成 configs、rules 并提交。

## 许可证

见 [LICENSE](LICENSE)。
