#!/usr/bin/env python3
"""根据 SITE 与可选 --doh 从 sites.yaml 选取站点配置并可选追加 DoH（本机证书），与 config.base 合并。"""
import argparse
import sys
from pathlib import Path

import yaml

# DoH 插件（本机证书，占位符由 entrypoint sed 替换）
DOH_PLUGIN = {
    "tag": "doh_server",
    "type": "http_server",
    "args": {
        "entries": [{"path": "/dns-query", "exec": "main_sequence"}],
        "listen": "0.0.0.0:8443",
        "cert": "__DOH_CERT__",
        "key": "__DOH_KEY__",
        "idle_timeout": 10,
    },
}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--site", required=True, help="sz / hk / dxb")
    ap.add_argument("--doh", action="store_true", help="追加 DoH（本机证书，需在容器内配置 DOH_CERT/DOH_KEY）")
    ap.add_argument("--output", default=None, help="输出路径（默认 configs/<site>[-doh].yaml）")
    args = ap.parse_args()

    root = Path(__file__).resolve().parent.parent
    base = yaml.safe_load((root / "config.base.yaml").read_text(encoding="utf-8"))
    sites = yaml.safe_load((root / "sites.yaml").read_text(encoding="utf-8"))

    site_name = args.site.strip().lower()
    if site_name not in sites:
        print(f"[merge_config] SITE={site_name} not in {list(sites.keys())}", file=sys.stderr)
        sys.exit(1)

    site_plugins = list(sites[site_name].get("plugins") or [])
    if args.doh:
        site_plugins.append(DOH_PLUGIN)
    base_plugins = base.get("plugins") or []
    merged = {**base, "plugins": site_plugins + base_plugins}

    out = Path(args.output) if args.output else root / "configs" / f"{site_name}{'-doh' if args.doh else ''}.yaml"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(yaml.dump(merged, allow_unicode=True, default_flow_style=False, sort_keys=False), encoding="utf-8")
    print(f"[merge_config] {site_name} doh={args.doh} -> {out}")


if __name__ == "__main__":
    main()
