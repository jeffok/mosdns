# 占位符与部署必填项

部署前在 **sites.yaml** 中替换占位符，并准备 DoH 证书。

---

## 1. 站点间 DNS 配置

由环境变量 **SITE** 选择站点（sz / hk / sgp / dxb），merge_config 自动根据 SITE 替换占位符。**站点间通信使用标准 DNS 端口 53**。

| SITE | 需要的 DNS 配置 | 默认值 | 说明 |
|------|----------------|--------|------|
| **sz** | `HKCLOUD_DNS_IP`、`SGPCLOUD_DNS_IP` | 10.100.50.222、100.64.89.1 | forward_global 和 forward_ai 使用内网 DNS |
| **hk** | `SGPCLOUD_DNS_IP` | 100.64.89.1 | forward_global 使用公网 DNS，forward_ai 使用内网 DNS |
| **sgp** | 无需配置 | - | 所有 forward 使用公网 DNS（1.1.1.1、8.8.8.8、9.9.9.9） |
| **dxb** | `HKCLOUD_DNS_IP` | 10.100.50.222 | forward_cn 使用内网 DNS，forward_global 和 forward_ai 使用公网 DNS |

在 `.env` 中配置这些 DNS IP，merge_config 会自动替换 sites.yaml 中的占位符。如不配置则使用默认值。

---

## 2. DoH 证书

由 MosDNS 内置 `http_server` 提供 `https://域名:8443/dns-query`，端口由 `DOH_PORT` 控制（默认 8443）。**证书不存在时 DoH 自动禁用**。

| 项目 | 说明 |
|------|------|
| 端口 | `.env` 中 `DOH_PORT=8443`（可改为 443；使用 `network_mode: host`，端口直接绑定宿主机，需防火墙放行） |
| 证书路径 | 在 `.env` 中配置 `DOH_CERT` 和 `DOH_KEY`，支持绝对路径或相对路径（相对路径相对于 `MOSDNS_DATA_DIR`） |
| 证书格式 | 支持 PEM 格式（`.pem`、`.crt`、`.key` 等），如 `./certs/fullchain.pem`、`/etc/ssl/certs/cert.crt` |
| 证书来源 | Let's Encrypt 或自有证书；续期后更新证书文件即可 |

---

## 3. 部署目录约定

**项目根目录**（docker-compose.yml 所在目录）下需有：

- `config.base.yaml`、`sites.yaml`
- `updater/` 目录（含 custom.txt、ai-list.txt、merge_config.py、gen_custom_rules.py、updater.py、daily_update.sh）
- DoH 证书（可选）：如需启用 DoH，在 `.env` 中配置 `DOH_CERT` 和 `DOH_KEY` 路径

启动后在同目录生成 `config.yaml`，规则生成到 **rules/**。证书不存在时 DoH 自动禁用，不影响标准 DNS 服务。

**自定义路径**：如需使用其他目录，可在 `.env` 中设置 `MOSDNS_DATA_DIR`。
