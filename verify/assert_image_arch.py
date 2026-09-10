#!/usr/bin/env python3
"""断言镜像 tar 里的 **二进制本身** 是目标架构。

为何不能只看 `docker image inspect --format '{{.Architecture}}'`：
    那个字段是 buildx 按 `--platform` 直接写进 image config 的，**与层里
    到底装了什么 ELF 无关**。若 Dockerfile 少了 `GOARCH=${TARGETARCH}`
    （编译阶段被 --platform=$BUILDPLATFORM 钉在构建机架构上），产出就是
    "config 写着 arm64、里面躺着 amd64 二进制" —— inspect 依然报 arm64。
    也就是说那条断言在真正会出错的那种回归面前恰好是空的。

    本脚本读 ELF header 的 e_machine（偏移 18，2 字节小端），这是编译器
    实际写下的目标架构，无法被镜像元数据伪装。

用法：
    assert_image_arch.py <image.tar> <期望 GOARCH> [二进制名 ...]

不依赖 docker/qemu：纯标准库解析 tar，可在任意架构的机器上校验任意架构的镜像。
"""

import io
import json
import sys
import tarfile

# ELF e_machine → Go GOARCH 命名
ELF_MACHINE = {0x03: "386", 0x28: "arm", 0x3E: "amd64", 0xB7: "arm64", 0xF3: "riscv64"}


def elf_arch(head: bytes):
    """从 ELF 头部前 64 字节解析架构；不是 ELF 则返回 None。"""
    if len(head) < 20 or head[:4] != b"\x7fELF":
        return None
    if head[5] != 1:  # EI_DATA：本项目目标平台均为小端，大端直接判未知
        return "big-endian-unsupported"
    return ELF_MACHINE.get(int.from_bytes(head[18:20], "little"), "unknown")


def scan(tar_path: str, wanted: set):
    """遍历外层 tar 的每个 blob，尝试当嵌套 tar（层）解开找目标二进制。

    对 tar 布局刻意不做假设：`--output type=docker` 在不同 buildx 版本下
    可能是 legacy 格式（<id>/layer.tar）或 OCI layout（blobs/sha256/<digest>），
    层可能 gzip 也可能不压缩。逐个 blob 试开销可忽略（层总量约 10MB），
    换来的是不会因为 buildx 换了导出布局就假绿或假红。
    """
    found, config_arch = {}, None
    with tarfile.open(tar_path) as outer:
        for member in outer.getmembers():
            if not member.isfile():
                continue
            blob = outer.extractfile(member).read()

            # image config 是个 JSON blob，带 architecture 字段——一并读出来，
            # 好让报告能同时给出"元数据声称"与"二进制实际"，出错时一眼看出是哪种。
            if blob[:1] == b"{":
                try:
                    doc = json.loads(blob)
                    if isinstance(doc, dict) and "architecture" in doc:
                        config_arch = doc["architecture"]
                except (ValueError, UnicodeDecodeError):
                    pass
                continue

            try:
                inner = tarfile.open(fileobj=io.BytesIO(blob), mode="r:*")
            except tarfile.TarError:
                continue
            with inner:
                for entry in inner.getmembers():
                    name = entry.name.rsplit("/", 1)[-1]
                    if entry.isfile() and name in wanted:
                        arch = elf_arch(inner.extractfile(entry).read(64))
                        if arch:
                            found[name] = arch
    return found, config_arch


def main():
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        return 2
    tar_path, expect = sys.argv[1], sys.argv[2]
    wanted = set(sys.argv[3:]) or {"sla-core", "collector", "migrate"}

    found, config_arch = scan(tar_path, wanted)
    print(f"镜像 config 声称架构：{config_arch}")

    missing = wanted - found.keys()
    if missing:
        print(f"::error::镜像里没找到二进制：{sorted(missing)}", file=sys.stderr)
        return 1

    bad = {n: a for n, a in sorted(found.items()) if a != expect}
    for name, arch in sorted(found.items()):
        print(f"  {'✅' if arch == expect else '❌'} /{name}  ELF={arch}")
    if bad:
        print(f"::error::二进制实际架构不是 {expect}：{bad}", file=sys.stderr)
        return 1

    print(f"✅ {len(found)} 个二进制的 ELF 目标架构均为 {expect}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
