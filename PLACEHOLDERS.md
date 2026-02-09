# 占位符与部署必填项

部署前在 **sites.yaml** 中替换占位符，并准备 DoH 证书。

---

## 1. 站点占位符（sites.yaml）

由环境变量 **SITE** 选择站点（sz / hk / sgp / dxb），merge_config 从 sites.yaml 取对应配置与 config.base.yaml 合并。将下列占位符换成实际隧道内网 IP，**站点间通信使用标准 DNS 端口 53**。

| 占位符 | 含义 | 出现在 |
|--------|------|--------|
| `<HKCLOUD_DNS_IN_TUNNEL_IP>` | szhome → hkcloud 的隧道内网 IP | sz `forward_global` |
| `<SGPCLOUD_DNS_IN_TUNNEL_IP>` | hkcloud/szhome → sgpcloud 的隧道内网 IP | sz、hk `forward_ai` |
| `<HKCLOUD_DNS_FOR_DXB_TUNNEL_IP>` | dxbhome → hkcloud（深圳侧）的隧道内网 IP | dxb `forward_cn` |

- **hk**：直接写 223.5.5.5、1.1.1.1 等；forward_ai 写 sgpcloud 隧道 IP:53
- **sgp**：全部公网 DNS 即可，无占位符
- **dxb**：仅 forward_cn 用 `<HKCLOUD_DNS_FOR_DXB_TUNNEL_IP>:53`

---

## 2. DoH 证书

由 MosDNS 内置 `http_server` 提供 `https://域名:8443/dns-query`，端口由 `DOH_PORT` 控制（默认 8443）。

| 项目 | 说明 |
|------|------|
| 端口 | `.env` 中 `DOH_PORT=8443`（可改为 443；使用 `network_mode: host`，端口直接绑定宿主机，需防火墙放行） |
| 证书路径 | 默认使用项目根目录下的 `certs/`，内含 `fullchain.pem`、`privkey.pem`；可在 `.env` 中设置 `DOH_CERT_DIR` 自定义 |
| 证书来源 | Let's Encrypt 或自有证书；续期后更新该目录即可 |

---

## 3. 部署目录约定

**项目根目录**（docker-compose.yml 所在目录）下需有：

- `config.base.yaml`、`sites.yaml`
- `updater/` 目录（含 custom.txt、ai-list.txt、merge_config.py、gen_custom_rules.py、updater.py、daily_update.sh）
- `certs/` 目录（内含 `fullchain.pem`、`privkey.pem`）

启动后在同目录生成 `config.yaml`，规则生成到 **rules/**。

**自定义路径**：如需使用其他目录，可在 `.env` 中设置 `MOSDNS_DATA_DIR` 和 `DOH_CERT_DIR`。
