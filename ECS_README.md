# mosdns ECS (EDNS Client Subnet) 配置说明

## 概述

mosdns v5.3+ 支持 `ecs_handler` 插件，可向上游 DNS 发送 ECS 信息，帮助 CDN 返回更优节点。

## 配置方式

通过环境变量 `ECS_PRESET` 设置预设 IP（为空则不发送 ECS）：

| 节点 | 建议值 | 说明 |
|------|--------|------|
| hkcloud | `119.29.29.29` | 中国 IP，出国/回国用户均需中国 CDN |
| dxbhome | `5.62.61.0` | 阿联酋 IP，回国用户多为中东地区 |

## 启用步骤（hkcloud/dxbhome ROS 容器）

1. **修改前**：按计划文档执行 ROS DNS 检查与切换（避免 mosdns 故障导致断网）
2. **添加环境变量**：
   ```routeros
   /container envs add list=ENV_MOSDNS key=ECS_PRESET value=119.29.29.29
   ```
3. **重启 mosdns 容器**：
   ```routeros
   /container stop [find name~mosdns]
   /container set [find name~mosdns] envlist=ENV_MOSDNS
   /container start [find name~mosdns]
   ```
4. **验证**：恢复 ROS DNS 指向 mosdns，确认解析正常

## 隐私说明

启用 ECS 会向公共 DNS 上游（如 1.1.1.1、8.8.8.8）发送客户端子网信息，涉及隐私。若使用 preset，则发送的是预设 IP 而非真实客户端 IP。
