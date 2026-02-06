#!/usr/bin/env python3
"""从 custom.txt 生成三份 custom 规则文件，并下载其余规则文件；仅在启动时执行，不预置文件。"""
import os
import re
import urllib.request
from pathlib import Path

SECTION = re.compile(r"^\s*\[(local|remote|hosts)\]\s*$", re.IGNORECASE)

# 与 rules-updater 相同的规则源，启动时一并下载
RULES_URLS = {
    "direct-list.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt",
    "china_ip_list.txt": "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt",
    "apple-cn.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt",
    "proxy-list.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt",
    "geosite-gfw.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt",
}


def parse(path: Path) -> dict[str, list[str]]:
    out: dict[str, list[str]] = {"local": [], "remote": [], "hosts": []}
    if not path.exists():
        return out
    current = None
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            s = line.strip()
            if not s or s.startswith("#"):
                continue
            m = SECTION.match(s)
            if m:
                current = m.group(1).lower()
                continue
            if current and current in out:
                out[current].append(line.rstrip("\n"))
    return out


def main() -> None:
    base = Path(__file__).resolve().parent
    rules_dir = Path(os.getenv("RULES_DIR") or (base.parent / "rules"))
    source = Path(os.getenv("CUSTOM_SOURCE") or (base / "custom.txt"))
    data = parse(source)

    rules_dir.mkdir(parents=True, exist_ok=True)
    for name, lines in [("local", data["local"]), ("remote", data["remote"]), ("hosts", data["hosts"])]:
        path = rules_dir / f"custom-{name}.txt"
        path.write_text("\n".join(lines) + ("\n" if lines else ""), encoding="utf-8")
    print("[gen_custom_rules] wrote custom-local.txt, custom-remote.txt, custom-hosts.txt")

    for name, url in RULES_URLS.items():
        path = rules_dir / name
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "mosdns-updater/1"})
            with urllib.request.urlopen(req, timeout=30) as r:
                path.write_bytes(r.read())
            print(f"[gen_custom_rules] downloaded {name}")
        except Exception as e:
            print(f"[gen_custom_rules] WARN download {name}: {e}")


if __name__ == "__main__":
    main()
