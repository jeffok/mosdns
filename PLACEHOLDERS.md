# 占位符与部署必填项

部署前在对应站点替换以下占位符（或在 RouterOS/系统里做策略路由），并准备 DoH 证书。

---

## 1. 站点配置与占位符（sites.yaml）

节点由环境变量 **SITE** 选择（sz / hk / sgp / dxb），merge_config 从 **sites.yaml** 中取对应站点与 config.base.yaml 合并生成 config.yaml。  
在 **sites.yaml** 里把下列占位符换成你真实隧道内网 IP（或域名），**端口统一用 5353**。

| 占位符 | 含义 | 出现在 |
|--------|------|--------|
| `<HKCLOUD_DNS_IN_TUNNEL_IP>` | szhome → hkcloud：hkcloud 上 MosDNS 对 szhome 暴露的隧道内网 IP（香港出口侧） | sites.yaml → sz（forward_global） |
| `<SGPCLOUD_DNS_IN_TUNNEL_IP>` | hkcloud / szhome → sgpcloud：sgpcloud 上 MosDNS 对 hk/sz 暴露的隧道内网 IP | sites.yaml → sz、hk（forward_ai） |
| `<HKCLOUD_DNS_FOR_DXB_TUNNEL_IP>` | dxbhome → hkcloud（深圳侧）：hkcloud 上 MosDNS 对 dxbhome 暴露的隧道内网 IP（建议走深圳出口） | sites.yaml → dxb（forward_cn） |

- **hk**：无需占位符，直接写 223.5.5.5 / 1.1.1.1 等；forward_ai 写 sgpcloud 隧道 IP:5353。  
- **sgp**：无需占位符，全部公网 DNS 即可。  
- **dxb**：仅 forward_cn 用 `<HKCLOUD_DNS_FOR_DXB_TUNNEL_IP>:5353`。

---

## 2. hkcloud 双出口（必做，否则 CDN 易错）

在 **hkcloud** 上做策略路由，让“解析用的上游 DNS 出口”和“分流类型”一致：

| 你需要的效果 | 做法（二选一或组合） |
|--------------|----------------------|
| **CN 域名** 用国内 DNS，且流量走 **深圳出口** | 把 223.5.5.5、119.29.29.29、114.114.114.114 等目的 IP 策略路由到深圳出口（routing table / gateway 或 mangle + mark-routing） |
| **非 CN/GFW** 用全球 DNS，且流量走 **香港出口** | 把 1.1.1.1、8.8.8.8、9.9.9.9 等目的 IP 策略路由到香港出口 |
| **AI** 走 sgpcloud | 把 sgpcloud 的隧道目的地址路由到能到 sgpcloud 的路径 |

具体用 **routing rule** 还是 **mangle + mark-routing** 按你现有 RouterOS/防火墙习惯即可；关键是“上游 DNS 的出口”和“分流策略”一致。

---

## 3. DoH 证书与域名（每站点必配）

由 **mosdns 内置 http_server** 提供 `https://dns-xxx.yourdomain.com:8443/dns-query`，端口由 `DOH_PORT` 控制（默认 8443，不占 443）。需 TLS 证书，手机建议用 **受信任证书**（Let’s Encrypt 或自有 CA），避免自签。

| 项目 | 说明 |
|------|------|
| **端口** | `.env` 中 `DOH_PORT=8443`（可改为 443；宿主机/防火墙需放行该端口） |
| **域名** | 每站点一个 DoH 域名，例如：dns-hk.xxx / dns-sgp.xxx / dns-sz.xxx / dns-dxb.xxx |
| **证书路径** | `DOH_CERT_DIR` 指向宿主机目录，内含 `fullchain.pem`、`privkey.pem`；compose 会挂到 mosdns 的 `/etc/mosdns/certs` |
| **证书来源** | Let’s Encrypt（dns-01 或 http-01）、或自有证书；续期后更新该目录即可 |

`.env` 示例：

```bash
SITE=sz
DOH_PORT=8443
DOH_CERT_DIR=/data/mosdns/certs
```

---

## 4. 部署目录约定

- **${MOSDNS_DATA_DIR}**（默认 `/data/mosdns`）下需有：
  - `config.base.yaml`
  - `sites.yaml`（各站点 forward/ai_domains，占位符按需替换；由 SITE 选择节点）
  - `updater/` 目录（含 custom.txt、ai-list.txt、gen_custom_rules.py、merge_config.py、updater.py）
- 启动后会在同目录生成：`config.yaml`；规则文件生成到 **rules/**（direct-list.txt、custom-*.txt、ai-list.txt 等）。

---

## 5. 执行顺序建议

1. **sgpcloud**：SITE=sgp，先跑通 MosDNS（DoH 能解析即可）。  
2. **hkcloud**：SITE=hk，配好双出口策略路由，再开 DoH。  
3. **szhome**：SITE=sz，把 `<HKCLOUD_DNS_IN_TUNNEL_IP>`、`<SGPCLOUD_DNS_IN_TUNNEL_IP>` 换成实际隧道 IP。  
4. **dxbhome**：SITE=dxb，把 `<HKCLOUD_DNS_FOR_DXB_TUNNEL_IP>` 换成 hkcloud 深圳侧隧道 IP。  
5. 可选：RouterOS 上 ai-list → updater 写 address-list=ai-sgp，再做 AI 流量策略路由。

---

## 6. 你需补的 6 个值（汇总）

| # | 内容 | 填到哪 |
|---|------|--------|
| 1 | hkcloud 深圳出口的路由表/网关或 mangle 方式 | RouterOS/防火墙 |
| 2 | hkcloud 香港出口的路由表/网关或 mangle 方式 | RouterOS/防火墙 |
| 3 | szhome → hkcloud 的隧道内网 IP（hkcloud 给 szhome 用） | sites.yaml → sz：`HKCLOUD_DNS_IN_TUNNEL_IP` |
| 4 | hkcloud → sgpcloud 的隧道内网 IP（sgpcloud 给 hk 用） | sites.yaml → sz、hk：`SGPCLOUD_DNS_IN_TUNNEL_IP` |
| 5 | dxbhome → hkcloud（深圳侧）隧道内网 IP | sites.yaml → dxb：`HKCLOUD_DNS_FOR_DXB_TUNNEL_IP` |
| 6 | 四站 DoH 域名（如 dns-hk.xxx / dns-sgp.xxx / dns-sz.xxx / dns-dxb.xxx） | 解析 + 证书 SNI；compose 仅用证书路径，域名在 DNS 和证书里配 |

补全上述 6 项并在 **sites.yaml** 中替换对应占位符、配置好 DoH 证书目录与 `DOH_PORT` 后，按当前 compose 即可“按 SITE 生成 config → 启动 MosDNS（含 DoH）”跑通整条链路。AI 域名解析出的 IP 由 updater 写入 RouterOS 的 `ROS_AI_LIST`（默认 `ai-sgp`），可做 AI 流量策略路由。

---

## 7. 本地 Docker 测试

- 规则文件生成在 **rules/** 目录（与 config.base.yaml 中路径一致）。
- 若本机 **5353 被占用**（如 macOS mDNS），测试脚本会自动选用 5354～5362 中空闲端口。
- 测试结束后会**自动删除**临时文件：`certs_test/`、`rules/`、`config.yaml`、`site.yaml`、`cache.dump`、`mosdns.log`。

在项目根目录执行（需已安装 Docker、openssl）：

```bash
./scripts/test.sh
```

如需手动测试并保留临时文件，可设环境变量后执行 `docker compose up -d`（5353 被占用时设置 `MOSDNS_LISTEN_PORT=5354` 等）：

```bash
MOSDNS_DATA_DIR="$(pwd)" DOH_CERT_DIR="$(pwd)/certs_test" SITE=sgp MOSDNS_LISTEN_PORT=5354 docker compose up -d
```
