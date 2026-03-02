#!/usr/bin/env python3
"""根据 SITE 从 sites.yaml 选取站点配置，与 config.base.yaml 合并生成 config.yaml。
仅 CI / 手动使用，运行时不再需要。"""
import argparse
import sys
from pathlib import Path

import yaml


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--site", required=True, help="sz / hk / dxb")
    ap.add_argument("--output", default=None, help="输出路径（默认 configs/<site>.yaml）")
    args = ap.parse_args()

    root = Path(__file__).resolve().parent.parent
    base_path = root / "config.base.yaml"
    sites_path = root / "sites.yaml"

    base = yaml.safe_load(base_path.read_text(encoding="utf-8"))
    sites = yaml.safe_load(sites_path.read_text(encoding="utf-8"))

    site_name = args.site.strip().lower()
    if site_name not in sites:
        print(f"[merge_config] SITE={site_name} not in {list(sites.keys())}", file=sys.stderr)
        sys.exit(1)

    site_plugins = sites[site_name].get("plugins") or []
    base_plugins = base.get("plugins") or []
    merged = {**base, "plugins": site_plugins + base_plugins}

    out = Path(args.output) if args.output else root / "configs" / f"{site_name}.yaml"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(yaml.dump(merged, allow_unicode=True, default_flow_style=False, sort_keys=False), encoding="utf-8")
    print(f"[merge_config] {site_name} -> {out}")


if __name__ == "__main__":
    main()
