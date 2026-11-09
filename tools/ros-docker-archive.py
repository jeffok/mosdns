#!/usr/bin/env python3
"""把 `docker save` 的 OCI 布局 tar 转成 RouterOS /container 能导入的 docker-archive tar。

用法：
  docker pull --platform linux/arm64 jeffok/mosdns:latest
  docker save jeffok/mosdns:latest -o mosdns-oci.tar
  python3 tools/ros-docker-archive.py mosdns-oci.tar mosdns-arm64.tar [repo:tag]

背景：新版 docker 的 save 输出是 OCI index + gzip 层，RouterOS 7.23 导入时报
「no config found in manifest」。这里把层解压为未压缩 tar，并生成只含单个镜像的 manifest.json。
"""
import gzip
import hashlib
import json
import os
import shutil
import sys
import tarfile
import tempfile


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    src_tar, out_tar = sys.argv[1], sys.argv[2]
    tag = sys.argv[3] if len(sys.argv) > 3 else "jeffok/mosdns:latest"
    with tempfile.TemporaryDirectory() as tmp:
        src, out = os.path.join(tmp, "src"), os.path.join(tmp, "out")
        os.makedirs(out)
        with tarfile.open(src_tar) as t:
            try:
                t.extractall(src, filter="data")
            except TypeError:  # Python < 3.12
                t.extractall(src)
        manifest = json.load(open(os.path.join(src, "manifest.json")))[0]
        cfg = open(os.path.join(src, manifest["Config"]), "rb").read()
        open(os.path.join(out, "config.json"), "wb").write(cfg)
        diffs = json.loads(cfg)["rootfs"]["diff_ids"]
        if len(diffs) != len(manifest["Layers"]):
            sys.exit("层数与 diff_ids 不一致")
        layers, written = [], set()
        for i, layer in enumerate(manifest["Layers"]):
            raw = open(os.path.join(src, layer), "rb").read()
            try:
                data = gzip.decompress(raw)
            except OSError:
                data = raw
            digest = hashlib.sha256(data).hexdigest()
            if "sha256:" + digest != diffs[i]:
                sys.exit(f"第 {i} 层校验失败")
            name = digest + ".tar"
            if name not in written:
                open(os.path.join(out, name), "wb").write(data)
                written.add(name)
            layers.append(name)
        json.dump([{"Config": "config.json", "RepoTags": [tag], "Layers": layers}],
                  open(os.path.join(out, "manifest.json"), "w"))
        with tarfile.open(out_tar, "w") as t:
            for f in sorted(os.listdir(out)):
                t.add(os.path.join(out, f), arcname=f)
    print(f"已生成 {out_tar}（{os.path.getsize(out_tar) // 1024 // 1024} MB）")


if __name__ == "__main__":
    main()
