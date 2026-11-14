# MosDNS 智能分流 DNS 服务

基于 [mosdns](https://github.com/IrineSistiana/mosdns) 的国内/国际/AI 三路分流 DNS 服务。
镜像地址：[jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)

本项目旨在提供一个开箱即用的 DNS 容器，所有差异均通过 `.env` 环境变量管理，适用于 **Docker Compose**、**Linux**、**NAS** 以及 **RouterOS 容器**。

## 核心场景

| 场景 | 说明 | 效果 |
| :--- | :--- | :--- |
| **日常分流** | 国内直连，代理走国际通道 | 提升国内解析速度，海外走代理 |
| **AI 专属分流** | 识别 GPT、Gemini、Claude 等 | **强制走 SGP 节点**，确保在受限地区可用 |
| **CDN 优化 (ECS)** | 开启 ECS 虚拟 IP | 根据 ECS 返回的 IP 获取更优的节点 |

## 规则更新机制

为了保持解析的精准度，本项目采用**动静结合**的规则更新策略：

1. **基础公共规则（静态内置）**：包含 direct-list、gfw-list 等 6 个核心文件。GitHub Actions 会在**每周一 04:00 (UTC+8)** 自动触发构建，下载最新的规则并推送至 Docker Hub (`latest` 标签)。你只需要定期 `pull` 即可。
2. **AI 规则列表（动态同步）**：容器内置 `sync-ai.sh`，**每 2 分钟**在后台自动从 GitHub (或自定义 API) 拉取最新 `ai-list`。列表变更后会立即重载 MosDNS，实现秒级生效，**无需重启容器**。
3. **本地定制（挂载保护）**：支持将 `rules/` 目录映射到宿主机。容器启动时会优先读取你挂载的文件，且不会被内置规则覆盖。

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

| 变量 | 默认值 | 说明 |
| :--- | :--- | :--- |
| **DNS_CN** | 119.29.29.29,223.5.5.5,114.114.114.114 | 国内域名上游 DNS，建议使用国内公共 DNS |
| **DNS_GLOBAL** | 1.1.1.1,8.8.8.8,9.9.9.9 | 国际域名上游 DNS |
| **DNS_AI** | 同 DNS_GLOBAL | AI 域名上游 DNS，如需走特定 SGP/专线时单独配置 |
| **TZ** | Asia/Shanghai | 容器时区 |
| **ECS_PRESET** | 空 | 开启 CDN 优化，填写目标地区的公网 IP 即可 |
| **DOH_ENABLED** | 0 | 设为 1 开启 DoH (DoH 证书需放 certs/ 目录下) |
| **DOH_CERT** | /etc/mosdns/certs/fullchain.pem | DoH 容器内证书路径 |
| **DOH_KEY** | /etc/mosdns/certs/privkey.pem | DoH 容器内私钥路径 |
| **ENABLE_ROS_ADDRLIST** | 0 | 设为 1 启用 ros_addrlist（RouterOS 容器专用，把海外解析 IP 写回 ROS 地址列表） |
| **ROS_API_ADDR** | 192.168.88.254:8728 | RouterOS API 地址（`ip service api`） |
| **ROS_API_USER** | 空 | ROS 专用账号（建议组仅 `api,read,write,test` + `address` 限容器 IP） |
| **ROS_API_PASS** | 空 | ROS 专用账号密码 |
| **AI_LIST_URL** | GitHub rules/ai-list.txt | AI 域名列表远端地址（支持自定义 API），默认每 2 分钟检查更新 |
| **CONTAINER_DNS** | 8.8.8.8 | 容器内部下载规则时使用的 DNS。默认使用 8.8.8.8 以保证 GitHub 可访问 |
| **RULE_FILE_MAX_AGE** | 82800 (23小时) | 规则文件保留秒数，超过此时间则触发更新，`0` 为每次强制下载 |
| **RELOAD_DELAY** | 0 | 每日 04:30 规则重载前的延迟秒数 |

---

## AI 分流配置说明

本项目内置了 AI 专用分流逻辑，自动识别 `chatgpt.com`, `gemini.google.com`, `claude.ai` 等域名并转发至 `DNS_AI` 对应的服务端。

### 1. Linux / NAS
直接启动容器，确保你的 `DNS_AI`（或 `DNS_GLOBAL`）上游指向你可用代理服务器的网关 IP（如新加坡节点 IP）即可。

### 2. RouterOS (ROS) 防火墙联动
在 ROS 环境中，利用防火墙地址列表（Address List）标记流量并强制走策略路由。

**ros_addrlist 插件**会把**海外域名**解析出的真实 IPv4 通过 **RouterOS API（8728）** 实时写入 `gfw-proxy` 地址列表，条目带 `timeout` 租约自动回收。

**部署步骤：**
1. **ROS 创建专用账号**：组仅含 `api,read,write,test`，并限制 `address=192.168.88.222/32`（只允许容器 IP 调用 API）。
   ```routeros
   /user/group/add name=mosdns-sync policy=api,read,write,test
   /user/add name=mosdns group=mosdns-sync password=<24位随机密码> address=192.168.88.222/32
   ```
2. **环境变量**：设 `ENABLE_ROS_ADDRLIST=1` + `ROS_API_USER`/`ROS_API_PASS`（其余参数默认值已内置，按需覆盖 `ROS_API_ADDR`/`ROS_ADDRLIST_NAME`/`ROS_ADDRLIST_TIMEOUT`/`ROS_ADDRLIST_DEDUP` 等）。
3. **策略路由**：ROS mangle 标记 `dst-address-list=gfw-proxy` 的流量，走代理出口（如 OVPN）。

> **注意**：mosdns 的 `has_resp_sequence` 已挂上该插件——凡产生应答的海外域名，解析后立刻回写 IP，CN 段与保留段自动过滤，同 IP 5 分钟内不重复写入。

---

## ECS 优化（CDN 节点优选）

ECS (EDNS Client Subnet) 允许 MosDNS 在向上游 DNS 发送查询时，携带一个模拟的客户端 IP。上游 CDN 会根据该子网信息，返回离该 IP 最近的节点。

### 为什么需要 ECS？
* **隐私保护**：本项目使用的是预设公网 IP，不发送你真实的内网 IP 或隐私 IP。
* **解决跨境解析错误**：当你的代理 DNS 服务在海外，但你想访问国内网站（如 B 站）并希望解析到国内 CDN 时，开启 ECS 能强行纠正 CDN 调度。

### 配置示例
在 `.env` 中设置 `ECS_PRESET`：

| 用户位置 | 建议配置 | 原理 |
| :--- | :--- | :--- |
| **海外回国用户** | `ECS_PRESET=119.29.29.29` | 上游以为你在中国，返回国内 CDN IP |
| **国内出国用户** | `ECS_PRESET=8.8.8.8` | 上游以为你在美国，返回海外 CDN IP |
| **回国在中东用户** | `ECS_PRESET=5.62.61.0` | 上游以为你在阿联酋，返回中东 CDN IP |

### 启用与验证
1. **添加变量**并重启容器：
   ```bash
   # Docker Compose
   ECS_PRESET=119.29.29.29

   # RouterOS 容器 (ROS)
   /container envs add list=ENV_MOSDNS key=ECS_PRESET value=119.29.29.29
   ```
2. **使用 `dig` 验证效果**：
   观察 `dig baidu.com` 返回的 IP 归属地是否符合预期。

---

## 常见问题

**1. 规则下载失败 (Connection refused/timeout)**
* **原因**：容器内部 DNS 无法解析 GitHub。
* **解决**：确保 `CONTAINER_DNS` 配置可用，默认值 `8.8.8.8` 通常可工作，若无法连接请更换其他 DNS。

**2. ROS 启动后所有查询返回 SERVFAIL**
* **原因**：ROS DNS 劫持规则捕获了 MosDNS 的 53 端口查询，形成回环。
* **解决**：在防火墙 NAT 规则中排除 MosDNS 容器 IP。

**3. AI 列表不更新**
* **解决**：确保容器能访问互联网。如果同步失败，MosDNS 会保留旧列表，通常不会导致服务失效。

**4. 多个站点同时重载导致服务卡顿**
* **原因**：多台设备/容器每天同时触发重载规则，可能导致下载源受限或网络拥堵。
* **解决**：通过配置 `RELOAD_DELAY` 参数，设置不同的重启时间。
  * 在 `.env` 中设置 `RELOAD_DELAY=120`（延迟 2 分钟重载）。每个站点设置不同的秒数（如 0, 120, 300），可平滑负载。
