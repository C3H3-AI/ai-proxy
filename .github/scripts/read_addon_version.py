#!/usr/bin/env python3
"""从 HA add-on 的 config.yaml 读取 version 字段并打印。

单独成文件而非内联 `python3 -c`：内联时引号要同时穿过
YAML 块标量 + shell 双引号 + Python 字符串三层，实测会因
['\\''] 这类写法直接 SyntaxError（已在本地复现）。

用法: read_addon_version.py <path-to-config.yaml>
退出码: 0 = 成功打印版本；1 = 找不到 version 字段
"""
import re
import sys

# 兼容 version: "1.1.0b13" / version: '1.1.0' / version: 1.2.3 / 多余空格
PATTERN = re.compile(r"""^version:\s*["']?([^"'\s#]+)""")


def read_version(path: str) -> str:
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            match = PATTERN.match(line)
            if match:
                return match.group(1)
    return ""


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: read_addon_version.py <config.yaml>", file=sys.stderr)
        return 1
    version = read_version(sys.argv[1])
    if not version:
        print(f"no version field found in {sys.argv[1]}", file=sys.stderr)
        return 1
    print(version)
    return 0


if __name__ == "__main__":
    sys.exit(main())
