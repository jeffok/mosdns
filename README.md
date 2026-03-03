# MosDNS 智能分流 DNS

基于 [mosdns](https://github.com/IrineSistiana/mosdns) 的国内/国际/AI 三路分流 DNS 服务。
镜像地址：[jeffok/mosdns](https://hub.docker.com/r/jeffok/mosdns)

所有站点共用同一个镜像，不同站点的差异通过 `.env` 环境变量配置。

## Docker Compose 部署

适用于 Linux 主机或 NAS。

```bash
mkdir -p mosdns/certs && cd mosdns

# 放入 docker-compose.yml 和 .env.example，然后：
cp .env.example .env
vi .env                # 按需修改 DNS 上游等参数

docker compose pull && docker compose up -d

# 测试
dig @127.0.0.1 baidu.com    # 应走国内 DNS
dig @127.0.0.1 google.com   # 应走国际 DNS
```

如果需要 DoH，在 `certs/` 目录放入证书文件，并在 `.env` 中设置 `DOH_ENABLED=1`。

## RouterOS Container 部署

适用于 MikroTik RouterOS 7.x 设备。

**1. 开启容器功能（只需一次，执行后重启路由器）：**

```routeros
/system/device-mode/update container=yes
/certificate/settings/set builtin-trust-anchors=trusted
/container/config/set registry-url=https://registry-1.docker.io tmpdir=disk1/tmp
```

**2. 创建虚拟网卡并加入网桥：**

```routeros
/interface/veth/add name=veth-mosdns address=192.168.8.252/24 gateway=192.168.8.254
/interface/bridge/port add bridge=br-lan interface=veth-mosdns
```

> 把 IP 和网桥名改成你自己的。

**3. 设置环境变量：**

```routeros
/container envs add list=ENV_MOSDNS key=DNS_CN value=119.29.29.29,223.5.5.5
/container envs add list=ENV_MOSDNS key=DNS_GLOBAL value=1.1.1.1,8.8.8.8
/container envs add list=ENV_MOSDNS key=TZ value=Asia/Shanghai
/container envs add list=ENV_MOSDNS key=CONTAINER_DNS value=8.8.8.8
```

> `CONTAINER_DNS` 是 RouterOS 容器必须设的，不然容器内部无法解析域名。
> 其他可选变量见下方环境变量表。

**4. 创建并启动容器：**

```routeros
/container add remote-image=jeffok/mosdns:latest interface=veth-mosdns \
  root-dir=disk1/images/mosdns envlist=ENV_MOSDNS name=mosdns \
  start-on-boot=yes logging=yes
/container start mosdns
```

**5. 把路由器 DNS 指向 mosdns：**

```routeros
/ip dns set servers=192.168.8.252
```

**6.（可选）自动拉起 — 容器崩溃时自动重启：**

```routeros
/system script add name=mosdns-watchdog source={ /container start mosdns }
/system scheduler add name=mosdns-watchdog interval=5m on-event=mosdns-watchdog
```

**7.（重要）如果路由器有 DNS 劫持规则，必须排除 mosdns IP：**

如果你配置了 `dstnat redirect dst-port=53` 这类规则把所有 DNS 流量劫持到路由器自身，
那 mosdns 的上游查询也会被劫持回来，形成死循环，导致所有查询返回 SERVFAIL。

解决方法：在劫持规则里把 mosdns 的 IP 排除掉。

```routeros
/ip/firewall/nat/set [find comment~"force lan dns"] src-address=!192.168.8.252
```

## 环境变量

复制 `.env.example` 为 `.env`，按需修改。


| 变量            | 默认值                                    | 说明                                                  |
| ------------- | -------------------------------------- | --------------------------------------------------- |
| DNS_CN        | 119.29.29.29,223.5.5.5,114.114.114.114 | 国内域名用的上游 DNS，逗号分隔                                   |
| DNS_GLOBAL    | 1.1.1.1,8.8.8.8,9.9.9.9                | 国际域名用的上游 DNS                                        |
| DNS_AI        | 同 DNS_GLOBAL                           | AI 域名用的上游 DNS（如需走特定出口）                              |
| TZ            | Asia/Shanghai                          | 时区                                                  |
| DOH_ENABLED   | 0                                      | 设为 1 开启 DoH，需要在 certs/ 放证书                          |
| DOH_CERT      | /etc/mosdns/certs/fullchain.pem        | DoH 证书路径（容器内）                                       |
| DOH_KEY       | /etc/mosdns/certs/privkey.pem          | DoH 私钥路径（容器内）                                       |
| ROS_HOST      | 空                                      | RouterOS SSH 地址（如 192.168.8.254:6220），用于同步 AI IP 列表 |
| ROS_USER      | admin                                   | RouterOS 用户名                                        |
| ROS_PASS      | 空                                      | RouterOS 密码                                         |
| CONTAINER_DNS | 空                                      | **仅 RouterOS 容器需要**，设为 8.8.8.8                      |
| RELOAD_DELAY  | 0                                      | 每日规则更新前的延迟秒数，多站点可以错开                                |


## 日常维护

- **规则自动更新**：容器每天凌晨 04:30 自动拉取最新规则并重载，不需要手动操作
- **AI 列表同步**：每 2 分钟把 AI 域名解析的 IP 同步到 RouterOS 的 address-list，用于策略路由
- **更新镜像**：Docker Compose 执行 `docker compose pull && docker compose up -d`；RouterOS 需要先停止删除旧容器，再重新创建

## 常见问题

**拉取镜像失败，报 SSL 证书错误**
→ 执行 `/certificate/settings/set builtin-trust-anchors=trusted`

**容器启动了但所有查询都返回 SERVFAIL**
→ 大概率是 DNS 劫持规则没有排除 mosdns IP，见上方第 7 步

**容器内下载规则文件失败**
→ RouterOS 容器启动后网络需要约 1-3 分钟才能通，entrypoint 会自动等待。镜像里也预下载了规则，首次启动不影响使用

**mounts 参数报错**
→ RouterOS 7.20 的 mounts 用 `name=` 不是 `list=`；如果 `mountlists` 参数不支持，直接把文件放到容器的 `root-dir` 对应路径下