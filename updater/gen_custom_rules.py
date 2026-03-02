#!/usr/bin/env python3
"""由 custom.txt 生成 custom-local.txt、custom-hosts.txt，复制 ai-list.txt。
仅 CI / 手动使用，运行时不再需要。"""
import re
from pathlib import Path

SECTION = re.compile(r"^\s*\[(local|hosts)\]\s*$", re.IGNORECASE)


def parse(path: Path) -> dict[str, list[str]]:
    out: dict[str, list[str]] = {"local": [], "hosts": []}
    if not path.exists():
        return out
    current = None
    for line in path.read_text(encoding="utf-8").splitlines():
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


def write_file(path: Path, lines: list[str], comment: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    body = "\n".join(lines) + ("\n" if lines else "")
    path.write_text(f"# {comment}\n{body}" if comment else body, encoding="utf-8", newline="\n")


def main() -> None:
    base = Path(__file__).resolve().parent
    rules_dir = base.parent / "rules"
    rules_dir.mkdir(parents=True, exist_ok=True)

    data = parse(base / "custom.txt")
    for name, key in [("local", "local"), ("hosts", "hosts")]:
        write_file(rules_dir / f"custom-{name}.txt", data[key], f"generated from custom.txt [{key}]")
    print("[gen_custom_rules] wrote custom-local.txt, custom-hosts.txt")

    ai_src = base / "ai-list.txt"
    ai_dst = rules_dir / "ai-list.txt"
    if ai_src.exists():
        lines = [ln.rstrip("\r\n") for ln in ai_src.read_text(encoding="utf-8").splitlines()]
        write_file(ai_dst, lines, "ai domains for forward_ai")
    else:
        write_file(ai_dst, [], "ai domains for forward_ai")
    print("[gen_custom_rules] wrote ai-list.txt")


if __name__ == "__main__":
    main()
