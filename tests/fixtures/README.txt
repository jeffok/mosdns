# 离线测试夹具：只保留能覆盖各条路径的最小规模规则
# 统计（供 validate 的相对下限参考，不是线上阈值）：
cn-domains          6
proxy-domains      3
gfw-domains        3
ai-domains         2
IPv4.China.list    3
cn-exceptions      2
apple-cn           1
# 实际内容见同目录 *.txt 与 *.list；IP 取文档保留段（192.0.2.0/24、198.51.100.0/24）
# 这些地址不会出现在真实流量里，避免误伤。