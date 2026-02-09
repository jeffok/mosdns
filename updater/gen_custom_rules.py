#!/usr/bin/env python3
"""由 custom.txt 生成 custom-*.txt、ai-list.txt，并下载远端规则文件。生成与下载均规范化；下载失败时保留原文件。"""
import os
import re
import time
import urllib.request
from pathlib import Path

SECTION = re.compile(r"^\s*\[(local|remote|hosts)\]\s*$", re.IGNORECASE)

# 规则源：文件名 -> 下载 URL（与 Loyalsoldier 等规则源一致）
RULES_URLS = {
    "direct-list.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt",
    "china_ip_list.txt": "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt",
    "apple-cn.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt",
    "proxy-list.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt",
    "geosite-gfw.txt": "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt",
}

DOWNLOAD_RETRIES = 3
DOWNLOAD_TIMEOUT = 30
USER_AGENT = "mosdns-updater/1"


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


def write_rule_file(path: Path, lines: list[str], comment: str) -> None:
    """规范化写入：UTF-8、Unix 换行、末尾单换行。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    body = "\n".join(lines) + ("\n" if lines else "")
    if comment:
        content = f"# {comment}\n{body}"
    else:
        content = body
    path.write_text(content, encoding="utf-8", newline="\n")


def download_file(name: str, url: str, dest: Path) -> bool:
    """下载到 dest；失败时保留原文件。返回是否写入成功。"""
    for attempt in range(1, DOWNLOAD_RETRIES + 1):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
            with urllib.request.urlopen(req, timeout=DOWNLOAD_TIMEOUT) as r:
                data = r.read()
            dest.write_bytes(data)
            return True
        except Exception as e:
            if attempt < DOWNLOAD_RETRIES:
                time.sleep(1)
                continue
            if dest.exists():
                print(f"[gen_custom_rules] WARN download {name} failed, kept existing file: {e}")
            else:
                print(f"[gen_custom_rules] WARN download {name} failed (no existing file): {e}")
            return False
    return False


def main() -> None:
    base = Path(__file__).resolve().parent
    rules_dir = Path(os.getenv("RULES_DIR") or os.getenv("MOSDNS_CONFIG_DIR") or str(base.parent))
    source = Path(os.getenv("CUSTOM_SOURCE") or (base / "custom.txt"))

    rules_dir.mkdir(parents=True, exist_ok=True)
    data = parse(source)

    # 生成 custom-*.txt（规范化）
    for name, key in [("local", "local"), ("remote", "remote"), ("hosts", "hosts")]:
        path = rules_dir / f"custom-{name}.txt"
        write_rule_file(path, data[key], f"generated from custom.txt [{key}]")
    print("[gen_custom_rules] wrote custom-local.txt, custom-remote.txt, custom-hosts.txt")

    # 生成 ai-list.txt（保留原文内容，仅规范化换行与头部）
    ai_src = base / "ai-list.txt"
    ai_dst = rules_dir / "ai-list.txt"
    if ai_src.exists():
        raw = ai_src.read_text(encoding="utf-8")
        lines = [ln.rstrip("\r\n") for ln in raw.splitlines()]
        write_rule_file(ai_dst, lines, "ai domains for forward_ai")
        print("[gen_custom_rules] wrote ai-list.txt")
    else:
        write_rule_file(ai_dst, [], "ai domains for forward_ai")
        print("[gen_custom_rules] wrote ai-list.txt (empty)")

    # 下载远端规则：失败则保留原文件
    for name, url in RULES_URLS.items():
        path = rules_dir / name
        if download_file(name, url, path):
            print(f"[gen_custom_rules] downloaded {name}")


if __name__ == "__main__":
    main()
