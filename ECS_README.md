# MosDNS ECS (EDNS Client Subnet) 配置说明

## 概述

EDNS Client Subnet (ECS) 是一种扩展协议，允许 DNS 客户端（MosDNS）在查询时附带一个客户端子网信息。
开启 ECS 后，上游 CDN DNS 将根据该子网返回地理位置更靠近目标节点的 IP。

## 为什么需要 ECS？

1.  **回国/回国优化**：身在海外，需要国内服务（B 站、百度网盘）解析到国内 CDN。
2.  **出国优化**：身在国内，需要海外服务（YouTube、GitHub）解析到海外 CDN。
3.  **隐私保护**：本项目使用的是 `ecs_handler` 的预设 IP 模式，不发送你真实的内网 IP，仅发送公网 IP，兼顾隐私与加速。

## 配置示例

在 `.env` 中配置 `ECS_PRESET`：

| 变量 | 建议值 | 适用场景 | 原理 |
|---|-------|---|---|
| `ECS_PRESET` | `119.29.29.29` | **海外回国用户** | 上游以为你在中国，返回国内 CDN IP |
| `ECS_PRESET` | `8.8.8.8` | **国内出国用户** | 上游以为你在美国，返回海外 CDN IP |
| `ECS_PRESET` | `5.62.61.0` | **回国在中东用户** | 上游以为你在阿联酋，返回中东 CDN IP |

## 启用步骤

1. **添加环境变量**：
   ```bash
   # 对于 Docker Compose
   ECS_PRESET=119.29.29.29

   # 对于 RouterOS 容器
   /container envs add list=ENV_MOSDNS key=ECS_PRESET value=119.29.29.29
   ```
2. **重启 mosdns 容器**。
3. **验证效果**：
   ```bash
   # 检查解析结果，观察返回的 IP 归属地
   dig baidu.com
   ```
