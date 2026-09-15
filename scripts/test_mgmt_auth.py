#!/usr/bin/env python3
"""验证管理接口鉴权（H2 修复）。

问题背景：config.yaml 默认 webui_user/pass 为空 → 面板登录未启用，
此时 _mgmt_authorized() 回退到按来源 IP 判断。原实现把**整个内网**
（192.168.* / 10.* / 172.16.* / 172.30.*）都视为可信，而 7870 端口
映射到宿主机 → 同网段任意设备可无认证调用管理接口。

修复后：未启用登录时只信任 **ingress 转发** 与 **本机回环**。

本脚本不启动服务，直接对鉴权函数做真值表验证。
用法: python3 scripts/test_mgmt_auth.py
"""
import sys

# ── 被验证的实现（从 login_ui.py 抽取的等价逻辑）──────────────────

def is_loopback(ip):
    return ip in ("127.0.0.1", "::1", "localhost", "")


def is_private(ip):
    if is_loopback(ip):
        return True
    return (ip.startswith("172.30.") or ip.startswith("172.16.")
            or ip.startswith("192.168.") or ip.startswith("10."))


def is_ingress(path):
    return path.startswith("/api/hassio_ingress/") or "/hassio_ingress/" in path


# 修复前
def old_mgmt_authorized(ip, path, webui_enabled, has_session):
    if has_session:
        return True
    if not webui_enabled:
        return is_private(ip)          # ← 整个内网都放行
    return False


# 修复后
def new_mgmt_authorized(ip, path, webui_enabled, has_session):
    if has_session:
        return True
    if webui_enabled:
        return False
    return is_ingress(path) or is_loopback(ip)


# ── 真值表 ────────────────────────────────────────────────────────

CASES = [
    # (说明, ip, path, webui_enabled, has_session, 期望新行为)
    ("ingress 转发（HA 已认证）", "127.0.0.1", "/api/hassio_ingress/TOK/api/accounts", False, False, True),
    ("本机回环直连", "127.0.0.1", "/api/accounts", False, False, True),
    ("容器内自调用", "172.30.32.1", "/api/accounts", False, False, False),  # 非 ingress 路径 → 拒
    ("LAN 直连 192.168.x", "192.168.1.50", "/api/accounts", False, False, False),
    ("LAN 直连 10.x", "10.0.0.9", "/api/accounts", False, False, False),
    ("公网直连", "203.0.113.7", "/api/accounts", False, False, False),

    ("已登录会话（ingress）", "127.0.0.1", "/api/hassio_ingress/TOK/api/accounts", False, True, True),
    ("已登录会话（LAN）", "192.168.1.50", "/api/accounts", False, True, True),

    ("启用登录 + 无会话（LAN）", "192.168.1.50", "/api/accounts", True, False, False),
    ("启用登录 + 无会话（ingress）", "127.0.0.1", "/api/hassio_ingress/TOK/api/accounts", True, False, False),
    ("启用登录 + 有会话", "192.168.1.50", "/api/accounts", True, True, True),
]


def main():
    print("=" * 78)
    print("管理接口鉴权真值表：修复前 vs 修复后")
    print("=" * 78)
    print(f"{'场景':<34} {'来源':<16} {'旧':<6} {'新':<6} {'期望':<6} 结果")
    print("-" * 78)

    failed = 0
    for desc, ip, path, wen, sess, want in CASES:
        old = old_mgmt_authorized(ip, path, wen, sess)
        new = new_mgmt_authorized(ip, path, wen, sess)
        ok = new == want
        if not ok:
            failed += 1
        mark = "✅" if ok else "❌"
        print(f"{desc:<34} {ip:<16} {str(old):<6} {str(new):<6} {str(want):<6} {mark}")

    print("-" * 78)
    print()
    print("关键差异（旧→新）：")
    print("  LAN 直连 192.168.x / 10.x : 旧=True（开放） → 新=False（拒绝）  ← 修复点")
    print("  ingress 转发              : 旧=True          → 新=True          （保留）")
    print("  本机回环                  : 旧=True          → 新=True          （保留）")
    print()

    if failed:
        print(f"❌ {failed} 个用例未达期望")
        return 1
    print(f"✅ 全部 {len(CASES)} 个用例通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
