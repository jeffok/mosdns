#!/usr/bin/env python3
"""根据 SITE 从 sites.yaml 选取站点配置，与 config.base.yaml 合并生成 config.yaml。"""
import os
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "pyyaml", "-q"])
    import yaml

def resolve_path(path_str: str, base_dir: Path) -> Path:
    """解析路径：绝对路径直接返回，相对路径相对于 base_dir"""
    path = Path(path_str)
    if path.is_absolute():
        return path
    return base_dir / path


def main() -> None:
    base_dir = Path(os.getenv("MOSDNS_CONFIG_DIR", "/etc/mosdns"))
    base_path = base_dir / "config.base.yaml"
    sites_path = base_dir / "sites.yaml"
    out_path = base_dir / "config.yaml"
    site_name = (os.getenv("SITE") or "sz").strip().lower()

    if not base_path.exists():
        print(f"[merge_config] missing {base_path}", file=sys.stderr)
        sys.exit(1)
    if not sites_path.exists():
        print(f"[merge_config] missing {sites_path}", file=sys.stderr)
        sys.exit(1)

    with open(base_path, "r", encoding="utf-8") as f:
        base = yaml.safe_load(f)
    with open(sites_path, "r", encoding="utf-8") as f:
        sites = yaml.safe_load(f)

    if not isinstance(sites, dict) or site_name not in sites:
        print(f"[merge_config] SITE={site_name} not found in {sites_path} (keys: {list(sites.keys()) if isinstance(sites, dict) else 'n/a'})", file=sys.stderr)
        sys.exit(1)

    site = sites[site_name]
    site_plugins = (site.get("plugins") or []) if isinstance(site, dict) else []
    base_plugins = base.get("plugins") or []
    
    # 创建 cache.dump 文件（如果不存在），避免首次启动时的错误日志
    cache_dump_path = base_dir / "cache.dump"
    if not cache_dump_path.exists():
        cache_dump_path.touch()
        print(f"[merge_config] created {cache_dump_path}")
    
    # 检查 DoH 证书是否存在
    doh_cert = os.getenv("DOH_CERT", "")
    doh_key = os.getenv("DOH_KEY", "")
    enable_doh = False
    
    if doh_cert and doh_key:
        cert_path = resolve_path(doh_cert, base_dir)
        key_path = resolve_path(doh_key, base_dir)
        if cert_path.exists() and key_path.exists():
            enable_doh = True
            print(f"[merge_config] DoH certificates found: cert={cert_path}, key={key_path}")
        else:
            print(f"[merge_config] WARN: DoH certificates not found (cert={cert_path}, key={key_path}), DoH disabled", file=sys.stderr)
    else:
        print(f"[merge_config] DoH certificates not configured (DOH_CERT/DOH_KEY), DoH disabled", file=sys.stderr)
    
    # 如果证书不存在，移除 doh_server 插件
    if not enable_doh:
        base_plugins = [p for p in base_plugins if p.get("tag") != "doh_server"]
    
    merged = {**base, "plugins": site_plugins + base_plugins}

    out_text = yaml.dump(merged, allow_unicode=True, default_flow_style=False, sort_keys=False)
    listen_port = os.getenv("MOSDNS_LISTEN_PORT", "53")
    doh_port = os.getenv("DOH_PORT", "8443")
    
    # 根据 SITE 替换站点间 DNS 占位符
    replacements = {}
    if site_name == "sz":
        # sz：需要 hkcloud_dns 和 sgpcloud_dns
        replacements["<HKCLOUD_DNS_IP>"] = os.getenv("HKCLOUD_DNS_IP", "10.100.50.222")
        replacements["<SGPCLOUD_DNS_IP>"] = os.getenv("SGPCLOUD_DNS_IP", "100.64.89.1")
    elif site_name == "hk":
        # hk：只需要 sgpcloud_dns
        replacements["<SGPCLOUD_DNS_IP>"] = os.getenv("SGPCLOUD_DNS_IP", "100.64.89.1")
    elif site_name == "sgp":
        # sgp：不需要替换（使用默认公网 DNS）
        pass
    elif site_name == "dxb":
        # dxb：需要 hkcloud_dns（与 sz 使用同一个变量）
        replacements["<HKCLOUD_DNS_IP>"] = os.getenv("HKCLOUD_DNS_IP", "10.100.50.222")
    
    # 替换站点间 DNS 占位符
    for placeholder, value in replacements.items():
        out_text = out_text.replace(placeholder, value)
    
    # 检查是否还有未替换的占位符
    import re
    remaining_placeholders = re.findall(r"<[A-Z_]+>", out_text)
    if remaining_placeholders:
        print(f"[merge_config] WARN: Unreplaced placeholders found: {set(remaining_placeholders)}", file=sys.stderr)
    
    # 替换端口和证书占位符
    out_text = out_text.replace("{{MOSDNS_LISTEN_PORT}}", listen_port)
    
    if enable_doh:
        cert_path = resolve_path(doh_cert, base_dir)
        key_path = resolve_path(doh_key, base_dir)
        out_text = (
            out_text.replace("{{DOH_PORT}}", doh_port)
            .replace("{{DOH_CERT}}", str(cert_path))
            .replace("{{DOH_KEY}}", str(key_path))
        )
    else:
        # 即使移除了插件，也要替换占位符避免配置错误
        out_text = out_text.replace("{{DOH_PORT}}", doh_port)
        out_text = out_text.replace("{{DOH_CERT}}", "")
        out_text = out_text.replace("{{DOH_KEY}}", "")

    with open(out_path, "w", encoding="utf-8") as f:
        f.write(out_text)

    print(f"[merge_config] SITE={site_name} -> {out_path}" + (", DoH enabled" if enable_doh else ", DoH disabled"))


if __name__ == "__main__":
    main()
