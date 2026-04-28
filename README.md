# MosDNS 智能分流 DNS 服务

基于 [mosdns](https://github.com/IrineSistiana/mosdns) 的国内/国际/AI 三路分流 DNS 服务。
镜像地址：[jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)

本项目旨在提供一个开箱即用的 DNS 容器，所有差异均通过 `.env` 环境变量管理，适用于 **Docker Compose**、**Linux**、**NAS** 以及 **RouterOS 容器**。

## 核心场景

| 场景 | 说明 | 效果 |
| :--- | :--- | :--- |
| **日常分流** | 国内直连，代理走国际通道 | 提升国内解析速度，海外走代理 |
| **AI 专属分流** | 识别 GPT、Gemini、Claude 等 | **强制走 SGP 节点**，确保在受限地区可用 |
| **CDN 优化** | 开启 ECS 虚拟 IP | 根据 ECS 返回的 IP 获取更优的节点 |

## 快速部署

### 1. Docker Compose (Linux / NAS)
创建目录并放置 `docker-compose.yml` 和 `.env`：
```bash
mkdir -p mosdns && cd mosdns
# 放置 docker-compose.yml 和 .env 后运行：
docker compose up -d
```

### 2. RouterOS 容器
ROS 容器中运行 MosDNS，需要将虚拟网卡加入网桥并配置环境变量（见下文详细配置）。

---

## 完整环境变量

复制 `.env.example` 为 `.env`，按需修改。

| 变量            | 默认值                                    | 说明                                                  |
| ------------- | -------------------------------------- | --------------------------------------------------- |
| **DNS_CN**        | 119.29.29.29,223.5.5.5,114.114.114.114 | 国内域名上游 DNS，建议使用国内公共 DNS |
| **DNS_GLOBAL**    | 1.1.1.1,8.8.8.8,9.9.9.9                | 国际域名上游 DNS                                     |
| **DNS_AI**        | 同 DNS_GLOBAL                           | AI 域名上游 DNS，如需走特定 SGP/专线时单独配置               |
| **TZ**            | Asia/Shanghai                          | 容器时区                                              |
| **ECS_PRESET**    | 空                                      | 开启 CDN 优化，填写目标地区的公网 IP 即可                       |
| **DOH_ENABLED**   | 0                                      | 设为 1 开启 DoH (DoH 证书需放 certs/ 目录下)              |
| **DOH_CERT**      | /etc/mosdns/certs/fullchain.pem        | DoH 容器内证书路径                                      |
| **DOH_KEY**       | /etc/mosdns/certs/privkey.pem          | DoH 容器内私钥路径                                    |
| **ROS_HOST**      | 空                                      | **ROS AI 同步**：路由器 REST API IP，需开启 `www` 服务       |
| **ROS_USER**      | admin                                   | ROS 管理员用户名                                       |
| **ROS_PASS**      | 空                                      | ROS 管理员密码                                       |
| **AI_LIST_URL**   | GitHub rules/ai-list.txt                | AI 域名列表远端地址（支持自定义 API），默认每 2 分钟检查更新            |
| **RELOAD_ON_AI_LIST_CHANGE** | 1                           | AI 列表变更后是否自动触发 MosDNS 重载                         |
| **CONTAINER_DNS** | 空                                      | **ROS 必须设置**：容器内部网络 DNS，否则无法更新规则             |
| **DOWNLOAD_DNS**  | 同 DNS_GLOBAL                           | 容器下载规则时临时指定的 DNS                              |
| **RULE_FILE_MAX_AGE** | 82800 (23小时)                       | 规则文件保留秒数，超过此时间则触发更新，`0` 为每次强制下载                 |
| **RELOAD_DELAY**  | 0                                      | 每日 04:30 规则重载前的延迟秒数                               |

---

## AI 分流配置说明 (含 ROS 同步)

本项目会自动将 AI 相关的域名（`chatgpt.com`, `gemini.google.com`, `claude.ai`, `grok.com` 等）分流到 `DNS_AI` 对应的服务端（如新加坡节点）。

### 1. Linux / NAS
直接启动容器，只要确保 `DNS_AI`（或 `DNS_GLOBAL`）指向的是你可用的**代理网关 IP**（如 SGP 节点 IP），AI 访问即正常。

### 2. RouterOS (ROS) 防火墙联动
在 ROS 环境中，通常使用防火墙地址列表（Address List）来标记流量并走策略路由。MosDNS 容器内置了同步功能，会自动将解析出的 AI IP 写入路由器。

**实现步骤：**
1. **开启 REST API**：确保路由器的 Web 界面可访问。
2. **配置 AI 解析出口 IP**：在 `.env` 中设置 `ROS_HOST=192.168.88.1`，`ROS_PASS=password`。
3. **自动生效**：容器每 2 分钟运行 `sync-ai.sh`，解析最新 IP 并写入 ROS。
4. **配置防火墙/策略路由**：在 ROS 中抓取 `address-list=ai-sgp` 的流量并强制指定路由出口。

> 这样，无论 AI 域名对应的 IP 怎么变，你的路由器都能实时跟踪并正确转发。

---

## ECS 优化（CDN 节点优选）

如果你开启了 ECS（如 `ECS_PRESET=119.29.29.29`），MosDNS 会在上游 DNS 查询时携带这个 IP。

| 用户位置 | ECS_PRESET 示例 | 用途 |
| :--- | :--- | :--- |
| **境外** | `119.29.29.29` | 请求国内 CDN 节点（加速 B 站、网盘等） |
| **境内** | `8.8.8.8` | 请求海外 CDN 节点（加速 GitHub、国际服务等） |

---

## 常见问题

**1. 规则下载失败 (Connection refused/timeout)**
* **原因**：ROS/容器内部 DNS 无法解析 GitHub。
* **解决**：配置 `DOWNLOAD_DNS=8.8.8.8`。

**2. ROS 启动后所有查询返回 SERVFAIL**
* **原因**：ROS DNS 劫持规则捕获了 MosDNS 的 53 端口查询，形成回环。
* **解决**：在防火墙 NAT 规则中排除 MosDNS 容器 IP。

**3. AI 列表不更新**
* **解决**：确保容器能访问互联网，检查 AI 解析的出口 IP 是否在 `.env` 中正确配置，或手动设置 `AI_LIST_URL` 指向一个可访问的私有 API。
