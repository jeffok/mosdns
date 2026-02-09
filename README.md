# MosDNS 智能分流 DNS

基于 [MosDNS](https://github.com/IrineSistiana/mosdns) 的多站点 DNS 分流方案，支持 **CN / Global / AI** 三分流，内置 DoH，并与 RouterOS address-list 联动做策略路由。

## 功能概览

- **三分流**：国内域名 → 国内 DNS；全球 / GFW → 全球 DNS；AI 域名 → 指定节点（如新加坡）
- **内置 DoH**：MosDNS `http_server`，端口可配置（默认 8443）
- **多站点**：sz（深圳家）、hk（香港云）、sgp（新加坡云）、dxb（迪拜家），由 `SITE` 选择
- **RouterOS 联动**：updater 将域名解析 IP 写入 address-list（remote 与 ai 分开），供策略路由使用
- **规则生成**：启动时下载/生成规则到 `rules/`，下载失败保留原文件
- **每日更新**：北京时间 5:00 自动更新规则并重启 mosdns（Docker Compose 部署）

## 目录结构

```
.
├── .github/workflows/  # CI：推送时自动构建并推送 updater 镜像至 Docker Hub
├── config.base.yaml    # 通用分流逻辑（不含 forward）
├── sites.yaml          # 各站点 forward/ai_domains，按 SITE 选择
├── docker-compose.yml
├── updater/
│   ├── merge_config.py     # 按 SITE 合并生成 config.yaml
│   ├── gen_custom_rules.py # 生成/下载规则到 rules/
│   ├── daily_update.sh     # 每日 5:00 北京时间更新规则并重启 mosdns
│   ├── updater.py          # 解析域名写入 ROS address-list
│   ├── custom.txt          # custom 规则源
│   ├── ai-list.txt         # AI 域名源
│   └── Dockerfile
├── scripts/
│   ├── test.sh             # 本地测试脚本
│   └── routeros-setup.rsc  # RouterOS 部署参考脚本（含注释，需按实际修改）
├── .env.example
└── PLACEHOLDERS.md     # 占位符与部署说明
```

## 镜像来源

| 镜像 | 来源 |
|------|------|
| mosdns | [irinesistiana/mosdns](https://hub.docker.com/r/irinesistiana/mosdns)（官方） |
| mosdns-updater | [jeffok/mosdns-updater](https://hub.docker.com/r/jeffok/mosdns-updater)，每次 push 到 main 或发布 release 时由 [GitHub Actions](.github/workflows/build.yml) 自动构建并推送，支持 amd64 / arm64 / armv7 |

**默认使用 Docker Hub 镜像**，无需本地编译；仓库同步时可本地编译使用。

## 配置 GitHub Actions（推送镜像至 Docker Hub）

在 GitHub 仓库 **Settings → Secrets and variables → Actions** 添加 Secret：

- `DOCKERHUB_TOKEN`：Docker Hub [Access Token](https://hub.docker.com/settings/security)（需 Create Repository 权限）

推送代码到 `main` 或发布 Release 后，会自动构建并推送至 `jeffok/mosdns-updater`。

## 快速开始（Docker Compose）

### 1. 准备

```bash
cp .env.example .env
# 编辑 .env：SITE、MOSDNS_DATA_DIR、DOH_CERT_DIR、ROS_* 等
```

确保 `${MOSDNS_DATA_DIR}` 下已有：
- `config.base.yaml`、`sites.yaml`（多站点需在 sites.yaml 中替换占位符，见 [PLACEHOLDERS.md](PLACEHOLDERS.md)）
- `updater/` 目录（本仓库整个 `updater/` 复制过去）
- DoH 证书：`${DOH_CERT_DIR}` 内含 `fullchain.pem`、`privkey.pem`

**SITE**：`sz` / `hk` / `sgp` / `dxb`，按当前站点选择。

### 2. 启动

**Docker Hub 镜像**（默认）：`docker compose pull && docker compose up -d`

**本地编译**：`docker compose up -d --build`

### 3. 验证

- DNS：`dig @127.0.0.1 -p 5353 google.com`
- DoH：`https://你的域名:8443/dns-query`

### 4. 每日规则更新（Docker Compose）

compose 已挂载 `/var/run/docker.sock`，无需额外配置。每日 5:00 北京时间 updater 自动更新规则并重启 mosdns。

---

## 在 RouterOS Container 中部署

RouterOS v7.4+ 支持 Container，可在设备上直接运行 MosDNS。**不支持 docker-compose**，需逐个添加容器。updater 可直接从 Docker Hub 拉取（`remote-image`），无需本地构建。支持每日 5:00 北京时间自动更新规则并重启 mosdns（见步骤 8）。

**参考脚本**：`scripts/routeros-setup.rsc` 含完整部署命令与注释，可按实际环境修改后分段执行；执行前需替换 `__LAN_IP__`、`__ROS_PASS__`、`__SITE__` 等占位符。

### 前提条件

- RouterOS v7.4+，已安装 **container** 包
- 外部存储（USB/SSD），≥100MB/s 读写
- 物理访问以启用 container 模式（默认关闭）

### 1. 启用 Container 模式并配置

```routeros
/system/device-mode/update container=yes
# 按提示重启后继续

/container/config/set registry-url=https://registry-1.docker.io tmpdir=disk1/tmp
```

### 2. 创建 veth 与桥接

```routeros
/interface/veth/add name=veth-mosdns address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=bridge-containers
/ip/address/add address=172.17.0.1/24 interface=bridge-containers
/interface/bridge/port add bridge=bridge-containers interface=veth-mosdns
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24
```

### 3. 准备数据目录

将项目文件上传到外部磁盘（如通过 Winbox、rsync）：

```
disk1/mosdns/
├── config.base.yaml
├── sites.yaml
├── updater/           # merge_config.py、gen_custom_rules.py、daily_update.sh、updater.py、custom.txt、ai-list.txt
└── certs/
    ├── fullchain.pem
    └── privkey.pem
```

### 4. 创建挂载与环境变量

```routeros
/container/mounts/add list=MOUNT_MOSDNS src=disk1/mosdns dst=/etc/mosdns
/container/mounts/add list=MOUNT_APP src=disk1/mosdns/updater dst=/app
/container/mounts/add list=MOUNT_CERTS src=disk1/mosdns/certs dst=/etc/mosdns/certs

/container/envs/add list=ENV_MOSDNS key=SITE value=sgp
# SITE 按当前站点改为 sz / hk / sgp / dxb
/container/envs/add list=ENV_MOSDNS key=MOSDNS_CONFIG_DIR value=/etc/mosdns
/container/envs/add list=ENV_MOSDNS key=RULES_DIR value=/etc/mosdns/rules
/container/envs/add list=ENV_MOSDNS key=MOSDNS_LISTEN_PORT value=5353
/container/envs/add list=ENV_MOSDNS key=DOH_PORT value=8443
/container/envs/add list=ENV_MOSDNS key=DOH_CERT_DIR value=/etc/mosdns/certs
/container/envs/add list=ENV_MOSDNS key=ROS_HOST value=172.17.0.1
/container/envs/add list=ENV_MOSDNS key=ROS_PORT value=8728
/container/envs/add list=ENV_MOSDNS key=ROS_USER value=admin
/container/envs/add list=ENV_MOSDNS key=ROS_PASS value=你的密码
```

> **ROS_HOST**：容器内访问 RouterOS API 用 veth 的 gateway `172.17.0.1`。

### 5. 添加并启动 updater 容器

从 Docker Hub 拉取预构建镜像：

```routeros
/container/add remote-image=jeffok/mosdns-updater:latest interface=veth-mosdns root-dir=disk1/images/mosdns-updater mountlists=MOUNT_MOSDNS,MOUNT_APP envlist=ENV_MOSDNS name=mosdns-updater start-on-boot=yes logging=yes
/container/start mosdns-updater
```

镜像内含 merge_config → gen_custom_rules → updater 流程，RouterOS 会自动匹配设备架构（arm/arm64/amd64）。等待生成 `rules/direct-list.txt` 等文件（可查看 `/container/print` 和日志）。

### 6. 添加并启动 mosdns

```routeros
/container/add remote-image=irinesistiana/mosdns:v5.3.3 interface=veth-mosdns root-dir=disk1/images/mosdns mountlists=MOUNT_MOSDNS,MOUNT_CERTS envlist=ENV_MOSDNS name=mosdns start-on-boot=yes logging=yes cmd="start -c /etc/mosdns/config.yaml"
/container/start mosdns
```

### 7. 端口转发与 DNS

将 LAN 的 5353、8443 转到容器 IP `172.17.0.2`（将 `192.168.88.1` 换成 RouterOS LAN IP）：

```routeros
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 dst-port=5353 protocol=udp to-addresses=172.17.0.2 to-ports=5353
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 dst-port=5353 protocol=tcp to-addresses=172.17.0.2 to-ports=5353
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 dst-port=8443 protocol=tcp to-addresses=172.17.0.2 to-ports=8443
```

客户端 DNS 指向 RouterOS LAN IP。若需 53 端口作为 DNS，可增加：

```routeros
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 dst-port=53 protocol=udp to-addresses=172.17.0.2 to-ports=5353
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 dst-port=53 protocol=tcp to-addresses=172.17.0.2 to-ports=5353
```

（将 `192.168.88.1` 换成 RouterOS LAN IP）

### 8. 每日规则更新与 mosdns 重启

updater 内 crond 会在 5:00 北京时间更新规则文件；RouterOS 无 docker socket，需用**系统 Scheduler** 在 5:05 执行重启（给 gen_custom_rules 留出时间）。

1. **设置时区**（若未设置）：

```routeros
/system/clock/set time-zone-name=Asia/Shanghai
```

2. **创建脚本**：

```routeros
/system/script/add name=mosdns-restart source={
  /container/restart mosdns
}
```

3. **创建定时任务**（每日 5:05 北京时间）：

```routeros
/system/scheduler/add name=mosdns-daily-restart interval=1d start-time=05:05:00 on-event=mosdns-restart
```

完成后：每日 5:00 updater 更新规则 → 5:05 RouterOS 重启 mosdns，规则生效。

### 9. 验证（RouterOS）

- 将客户端 DNS 指向 RouterOS LAN IP（如 192.168.88.1）
- `dig @192.168.88.1 -p 5353 google.com`（或 `dig @LAN_IP -p 53` 若已配置 53 转发）
- DoH：`https://你的域名:8443/dns-query`

---

## 本地测试

```bash
./scripts/test.sh
```

脚本会：自动选择空闲端口（5353 被占用时使用 5354～5362）、生成规则、启动服务、执行 DNS 查询，结束后删除临时文件。使用 `--build` 保证测试最新代码。

---

## 更多说明

- **占位符与部署细节**：见 [PLACEHOLDERS.md](PLACEHOLDERS.md)
- **环境变量**：见 [.env.example](.env.example)
- **RouterOS 参考脚本**：见 [scripts/routeros-setup.rsc](scripts/routeros-setup.rsc)，含完整部署命令与注释
- **更新镜像**：Compose 执行 `docker compose pull && docker compose up -d`；RouterOS 需 `/container/stop mosdns-updater`、`/container/stop mosdns` 后删除容器并重新添加（或使用 `remote-image` 拉取新镜像后重建）
- **许可证**：见 [LICENSE](LICENSE)
