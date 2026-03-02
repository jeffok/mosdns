# MosDNS 智能分流 DNS

基于 [MosDNS](https://github.com/IrineSistiana/mosdns) 的 CN / Global / AI 三分流，与 RouterOS `ai-sgp` address-list 联动做策略路由。单容器自更新，镜像 [jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)。

## 快速开始

**Docker Compose（hk/sz）**

```bash
cp configs/sz.yaml config.yaml
echo 'ROS_HOST=10.100.50.254:6220' >> .env
echo 'ROS_PASS=你的密码' >> .env
docker compose pull && docker compose up -d
```

**RouterOS Container（dxb）**：将 `configs/dxb.yaml` 改名为 `config.yaml`，与 `rules/` 上传到 `disk1/mosdns/`，按 `scripts/routeros-setup.rsc` 执行（替换 `__LAN_IP__`、`__TZ__`）。

## 目录与用途

| 路径 | 用途 |
|------|------|
| `configs/sz.yaml` `hk.yaml` `dxb.yaml` | 各站点最终配置，部署时复制为 `config.yaml` |
| `rules/` | 静态规则（CI 生成）；外部规则由 entrypoint 循环内 wget 下载（每日 04:30 重载时再拉一次） |
| `updater/custom.txt` `ai-list.txt` | 自定义/AI 域名源，改后 push 触发 CI 重新生成 |
| `sites.yaml` `config.base.yaml` | 开发用源，改后 push 触发 CI 重新生成 configs |

hk 启用 DoH 时，在项目根目录建 `certs/`，放入 `fullchain.pem`、`privkey.pem`（与 `sites.yaml` 中路径一致）。

## 环境变量（Compose）

| 变量 | 默认 | 说明 |
|------|------|------|
| TZ | Asia/Shanghai | 时区，crond 04:30 重载规则（仅重启 mosdns 进程） |
| ROS_HOST | 空 | RouterOS SSH，格式 `host` 或 `host:port`，空则不同步 |
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
