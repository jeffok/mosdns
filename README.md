# MosDNS 智能分流 DNS 服务

基于 [mosdns](https://github.com/IrineSistiana/mosdns) 的国内/国际/AI 三路分流 DNS 服务。
镜像地址：[jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)

本项目旨在提供一个开箱即用的 DNS 容器，所有差异均通过 `.env` 环境变量管理，适用于 **Docker Compose**、**Linux**、**NAS** 以及 **RouterOS 容器**。

## 快速开始

### 1. 准备目录

创建配置文件存储目录，例如 `mosdns/`：

```bash
mkdir -p mosdns && cd mosdns
```

### 2. 准备配置文件

下载 `docker-compose.yml` 和 `.env.example`：

```bash
cp .env.example .env
vi .env  # 根据实际情况修改
```

### 3. 启动服务

使用 Docker Compose 启动容器：

```bash
docker compose up -d
```

> **注意**：启动过程中，容器会自动下载初始规则文件。首次启动可能需要 1-3 分钟。

## 核心功能

本项目实现了智能的路由分流，确保 DNS 查询速度最优化：

1.  **国内域名直连**：识别 `direct-list.txt` 中的域名，直接转发至 `DNS_CN`（国内公共 DNS）。
2.  **国际/AI 域名代理**：识别 `proxy-list.txt`、`gfw.txt` 或 AI 域名列表，转发至 `DNS_GLOBAL` 或 `DNS_AI`。
3.  **缓存优化**：内置 Lazy-Cache (惰性缓存)，即使上游失效，也能保证已解析的域名在一定时间内可用。
4.  **ECS 支持**：支持通过环境变量 `ECS_PRESET` 指定虚拟子网，优化 CDN 解析。

## 配置说明 (环境变量)

复制 `.env.example` 为 `.env`，你可以根据网络环境自由组合以下参数。

| 变量 | 默认值 | 说明 |
| ------------- | ------------- | ------------- |
| **DNS_CN** | 119.29.29.29,223.5.5.5 | 国内域名上游，建议填写 ISP 提供的 DNS 或国内公共 DNS |
| **DNS_GLOBAL** | 1.1.1.1,8.8.8.8 | 国际/代理域名上游，建议使用境外 DNS 服务 |
| **DNS_AI** | 同 DNS_GLOBAL | AI 域名专用上游，如需走特定出口/专线时可单独指定 |
| **TZ** | Asia/Shanghai | 系统时区 |
| **ECS_PRESET** | 空 (不启用) | **优化 CDN 关键参数**：填入一个目标地区的公网 IP (如 `119.29.29.29`) |

### ECS 使用示例

| 使用场景 | 推荐配置 | 效果 |
| :--- | :--- | :--- |
| **回国场景** (人在海外) | `ECS_PRESET=119.29.29.29` | 访问国内服务（B站/微博）解析到国内 CDN |
| **出国场景** (人在国内) | `ECS_PRESET=1.1.1.1` | 访问国际服务（YouTube/Twitter）解析到海外 CDN |

## 进阶功能选项

### DoH (DNS over HTTPS)
在 `certs/` 目录下放置证书文件，并开启：
```env
DOH_ENABLED=1
```

### RouterOS 同步 (ROS API)
将解析到的 AI IP 自动同步回路由器防火墙规则：
```env
ROS_HOST=192.168.88.1
ROS_PASS=mysecretpass
```

### 下载与缓存管理
```env
DOWNLOAD_DNS=8.8.8.8       # 强制指定容器内部下载规则的 DNS
RULE_FILE_MAX_AGE=82800    # 规则更新间隔 (默认 23 小时)
```

## 部署注意事项

**Linux / NAS 用户**
如果你的主机开启了防火墙或 53 端口被占用，请确保释放 53 端口。如需挂载本地规则目录进行定制，可以添加 `- ./rules:/etc/mosdns/rules` 的 Volume 映射。

**RouterOS (ROS) 用户**
1. 必须设置 `CONTAINER_DNS`（如 `8.8.8.8`），否则容器内无法联网更新规则。
2. 确保防火墙 NAT 规则排除了 MosDNS 容器的 IP，避免形成 DNS 解析死循环。
3. `AI_LIST_URL` 的同步依赖于容器网络通畅，建议初次启动时耐心等待。

## 常见问题

**1. 容器启动后日志显示下载失败 (Connection refused/timeout)**
*   **原因**：容器内部的 DNS 无法解析 GitHub 地址。
*   **解决**：在 `.env` 中配置 `DOWNLOAD_DNS=8.8.8.8`。

**2. 查询所有域名都返回失败 (SERVFAIL)**
*   **原因**：上游查询被路由器 DNS 劫持规则捕获，导致回环。
*   **解决**：在防火墙规则中，排除 MosDNS 容器 IP 的 53 端口流量。

**3. 规则更新失败**
*   **解决**：检查 `RULE_FILE_MAX_AGE` 设置，或者手动设置 `DOWNLOAD_DNS` 为可靠的 DNS。
