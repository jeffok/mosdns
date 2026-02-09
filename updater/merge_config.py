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
    merged = {**base, "plugins": site_plugins + base_plugins}

    out_text = yaml.dump(merged, allow_unicode=True, default_flow_style=False, sort_keys=False)
    listen_port = os.getenv("MOSDNS_LISTEN_PORT", "53")
    doh_port = os.getenv("DOH_PORT", "8443")
    doh_cert_dir = os.getenv("DOH_CERT_DIR", "/etc/mosdns/certs")
    out_text = (
        out_text.replace("{{MOSDNS_LISTEN_PORT}}", listen_port)
        .replace("{{DOH_PORT}}", doh_port)
        .replace("{{DOH_CERT_DIR}}", doh_cert_dir)
    )

    with open(out_path, "w", encoding="utf-8") as f:
        f.write(out_text)

    print(f"[merge_config] SITE={site_name} -> {out_path}")


if __name__ == "__main__":
    main()
