# BGP 验证服务

本目录是与现有 mosdns 并行运行的验证服务，不是 mosdns 全功能替代品。它从配置的全球上游解析，逐个排除 CN、私网与保留 IPv4，然后通过内嵌 GoBGP 发布 `/32`。AAAA 暂时返回空应答，与 szhome 当前禁用 IPv6 的范围一致。它不持有 RouterOS 管理账号或密码，也不调用 RouterOS REST API。

家庭 DNS 在验证期间使用 `119.29.29.29`。BGP 学习路由仅进入独立测试表，只允许明确的测试来源查询该表；默认路由、家庭客户端 DNS 和现有 mosdns 不由这个镜像改写。切到国内 DNS 主要保障国内访问，不代表海外网站继续可用。

## 构建与导出

从仓库根目录执行：

```sh
sh tools/build-bgp-agent.sh
```

默认构建 `linux/arm64`，固定标签为 `jeffok/mosdns-bgp:bgp-validation-20261114.1`。输出位于 `.cache/bgp-agent/bgp-validation-20261114.1/`，包含 RouterOS 可导入的 `bgp-agent-arm64.tar`、OCI 归档、镜像 ID、配置示例、源码哈希与 SHA256 清单。脚本只构建本机镜像和导出文件，不推镜像、不连接或修改路由器。可设置 `PLATFORM=linux/amd64` 构建本机验证版本。

Dockerfile 锁定 Go 1.26.3 和 Alpine 基础镜像的多架构索引摘要，模块版本与校验和由 `go.mod`、`go.sum` 固定。单元测试在构建机原生架构执行，只有 `go build` 使用目标架构；关闭 CGO，不需要 QEMU 编译。构建上下文仅为本目录，配置、CN 规则和路由状态都在运行时挂载，不包含项目 `key.ini` 等凭据。

## 配置与目录

复制 `config.example.json` 到运行时挂载的 `/etc/bgp-agent/config.json`，同时将已验证的国内网段文件挂载到 `/etc/bgp-agent/IPv4.China.list`。`/data` 必须为持久卷，并且不能与原 mosdns 的状态目录混用。

示例验证地址为 `192.168.88.223/24`，网关和 BGP 邻居为 `192.168.88.254`，本地 AS 64513，ROS AS 64512。镜像启动前需核对地址未占用。ROS 入站过滤器必须把接收路由的下一跳固定到 OVPN，并确认实际 `active` 状态与 `immediate-gw`。示例发布的 `next_hop` 是服务自身地址；未配置 ROS 下一跳修正时，网站流量可能误送进容器。

| 参数 | 验证初值 | 行为 |
| --- | --- | --- |
| `ttl_max` | 300 秒 | 保留上游较短 TTL，缓存命中递减，不提供过期缓存 |
| `retain_seconds` | 86400 秒 | 业务路由保留时间，不是 BGP Hold Time |
| `confirm_ms` | 2500 毫秒 | 等待 ROS active-route 回报的上限，不是固定睡眠 |
| `ack_max_age_ms` | 3000 毫秒 | 超时回报不再视为可用；异常时新查询返回 SERVFAIL |
| `max_routes` | 512 | 容量满时拒绝继续发布，不能驱逐有效路由凑空间 |
| `gc_dry_run` | true | 记录回收候选，不自动撤掉正常站点路由 |

预解析列表只保留少量实际子域。刷新按有效 TTL 的约 80% 调度，最长 240 秒并分散时刻；前次失败会重试，不依据一次解析失败就撤掉旧路由。IP 保存跨域名引用以及最后一次对外应答到期时间。真正回收需同时满足引用到期、保留期到期和已发 DNS 应答到期后 5 分钟余量；独立测试时才将 `gc_dry_run` 设为 false。

GoBGP 会话当前使用 Keepalive 10 秒、Hold Time 30 秒。BGP 路由没有逐条 TTL，只有显式撤销、邻居/下一跳失效等事件改变其有效性。验证版未启用 Graceful Restart，不应假定服务退出后 ROS 保留路由。

## ROS 回报与首次连接

仅 `AddPath` 成功和 BGP `Established` 都不能证明 ROS 转发表就绪。服务提供：

- `GET /desired`：待核对的前缀以及当前会话 epoch。
- `POST /ack`：ROS 核对独立测试表中的 active 路由与 OVPN 下一跳后，批量回报已就绪前缀；只接受配置中 `peer_ip` 的来源。
- `GET /state`：状态与计数；其中本地发布记录本身不等于转发表安装成功。

ROS 本地定时脚本负责回报，不需要让服务登录 ROS。ROS 防火墙还必须限制 HTTP 9080 仅接受路由器本身以及必要的诊断来源，TCP 179 只接受固定邻居。该回报证明 ROS 路由表状态；首个 SYN 的实际出口仍要通过测试连接验证。旧 epoch、邻居重连前的 ACK 和超过有效时间的 ACK 不会放行新查询。

示例探测命令：

```sh
/usr/bin/bgp-agent probe -dns 127.0.0.1:53 -name github.com -url https://github.com/
/usr/bin/bgp-agent probe -dns 127.0.0.1:53 -name www.baidu.com -url ""
```

HTTPS 验证使用正常证书校验。镜像包含 CA 信任库，不通过跳过证书校验来判定访问成功。

## 验证与回退边界

至少验证预热命中、陌生 IP 首次访问、缓存命中、服务重启、邻居断开重连、跨域名共享 IP、候选回收及专用测试 IP 撤路。测试表不得有掩盖 BGP 失效的 OVPN 默认路由，测试流量不得被旧 REST 地址池规则或 FastTrack 接管。控制用的 DNS 上游路由需单独核对，避免测试策略把上游查询送错出口。

验证失败时，停止新容器和 ROS 回报任务，撤销仅属于 BGP 验证的路由策略、邻居及测试表；家庭 DNS 继续使用 `119.29.29.29`。不清空全网 conntrack，不删除 OVPN 证书，不把旧连接改标记后当成首次访问成功。完整上线前还需处理国内域名策略、来源一致性、客户端自带 DNS、映射/路由容量及长连接撤路影响，不能将本镜像作为全家长期 DNS 直接切换。
