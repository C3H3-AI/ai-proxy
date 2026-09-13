#!/bin/bash
set -e

# ============================================================
# AI Proxy HA Addon - 启动脚本
# 双平台（WorkBuddy + TraeWork）无头 server 与管理 Web UI 全部交由
# login_ui.py 统一托管（生成 config、热重启 serverd、扫码登录、账号管理）。
# ============================================================

# 持久化目录
mkdir -p /data/auths /data/data
chmod 700 /data/auths /data/data 2>/dev/null || true

echo "[AI-Proxy] Starting management panel (login_ui.py)..."
echo "[AI-Proxy]   Internal serverd port: 7864"
echo "[AI-Proxy]   Public/Ingress port:    7870 (OpenAI API + 管理 Web UI)"
echo "[AI-Proxy]   auth_dir: /data/auths (WorkBuddy / TraeWork 登录)"

exec python3 /app/login_ui.py