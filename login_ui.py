#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# AI Proxy 管理面板 + OpenAI 兼容 API 统一入口。
#
# 架构：
#   - login_ui.py（本文件）     监听 0.0.0.0:7863，提供管理 Web UI + 代理 /v1/* 到 serverd
#   - serverd（Go 双平台无头）  监听 0.0.0.0:7864，WorkBuddy + TraeWork 双平台上游
#   - ctl（Go）                 账号列表/积分/签到/刷新令牌（面板经子进程调用）
#   - login（Go）               WorkBuddy 登录 url/poll
#   - logintrae（Go）           TraeWork 登录 url/poll
import datetime
import http.server
import json
import os
import signal
import subprocess
import threading
import time
import urllib.parse
import urllib.request

# ---------------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------------
HOST = "0.0.0.0"
PORT = 7870
SRVD_PORT = 7864
SRVD_UPSTREAM = "http://127.0.0.1:%d" % SRVD_PORT

APP_DIR = "/app"
AUTH_DIR = "/data/auths"
STATE_DIR = "/data/data"
STATE_FILE = os.path.join(STATE_DIR, "state.json")
CONFIG_FILE = "/data/config.json"
OPTIONS_FILE = "/data/options.json"
TOKEN_FILE = os.path.join(STATE_DIR, "config.token")

SRVD_BIN = os.path.join(APP_DIR, "serverd")
CTL_BIN = os.path.join(APP_DIR, "ctl")
LOGIN_BIN = os.path.join(APP_DIR, "login")
LOGINTRAE_BIN = os.path.join(APP_DIR, "logintrae")
LOGINQODER_BIN = os.path.join(APP_DIR, "loginqoder")

TRAE_STATE = "/tmp/ai-proxy-trae-login-state.json"
QODER_STATE = "/tmp/ai-proxy-qoder-login-state.json"
SRVD_LISTEN = "0.0.0.0:%d" % SRVD_PORT

DEFAULT_OPTIONS = {
    "api_key": "",
    "webui_user": "admin",
    "webui_pass": "admin",
    "region": "cn",
    "cooldown_hard_credit": "12h",
    "cooldown_soft_rate": "60s",
    "cooldown_err_threshold": 3,
    "cooldown_err_cooldown": "10m",
    "checkin_times": "09:00,21:00",
    "keepalive_hours": [22],
    "upstream_timeout": 120,
}

PROXY_PREFIX = "/v1/"
V1_RAW = ["/v1/models", "/status", "/healthz"]

WEBUI_COOKIE = "ai_proxy_session"
SESSION_FILE = "/tmp/ai-proxy-webui-session"


def _webui_enabled():
    """Web UI 面板是否启用账号登录。"""
    o = load_options()
    return bool((o.get("webui_user") or "").strip() and (o.get("webui_pass") or "").strip())


def _webui_check_cookie(cookie_val):
    """校验面板会话 cookie。"""
    if not cookie_val:
        return False
    try:
        with open(SESSION_FILE, "r", encoding="utf-8") as f:
            return f.read().strip() == cookie_val.strip()
    except Exception:
        return False


def _webui_new_session():
    """生成并持久化一个新会话 token。"""
    import hashlib, time
    tok = hashlib.sha256(("%f|%d" % (time.time(), id(SESSION_FILE))).encode()).hexdigest()
    try:
        with open(SESSION_FILE, "w", encoding="utf-8") as f:
            f.write(tok)
    except Exception:
        pass
    return tok



# ---------------------------------------------------------------------------
# 通用工具
# ---------------------------------------------------------------------------
def now_str():
    return datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")


def load_options(fresh=False):
    opts = dict(DEFAULT_OPTIONS)
    try:
        if os.path.isfile(OPTIONS_FILE):
            with open(OPTIONS_FILE, "r", encoding="utf-8") as f:
                data = json.load(f)
            if isinstance(data, dict):
                for k in DEFAULT_OPTIONS:
                    if k in data and data[k] is not None:
                        opts[k] = data[k]
    except Exception:
        pass
    return opts


def save_options(opts):
    os.makedirs(os.path.dirname(OPTIONS_FILE), exist_ok=True)
    tmp = OPTIONS_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(opts, f, indent=2, ensure_ascii=False)
    os.replace(tmp, OPTIONS_FILE)


def normalize_checkin_times(v):
    if isinstance(v, list):
        return [str(x) for x in v]
    if isinstance(v, str):
        return [x.strip() for x in v.replace("，", ",").split(",") if x.strip()]
    return ["09:00", "21:00"]


def normalize_hours(v):
    if isinstance(v, list):
        return [int(x) for x in v]
    if isinstance(v, str):
        return [int(x) for x in v.replace("，", ",").split(",") if str(x).strip()]
    return [22]


def build_config(opts):
    """按 workbuddy-wild 的 config schema 生成 serverd 配置 JSON。"""
    return {
        "listen": {"host": "0.0.0.0", "port": SRVD_PORT},
        "api_key": opts.get("api_key", ""),
        "auth_dir": AUTH_DIR,
        "state_file": STATE_FILE,
        "region": opts.get("region", "cn"),
        "cooldown": {
            "hard_credit": opts.get("cooldown_hard_credit", "12h"),
            "soft_rate": opts.get("cooldown_soft_rate", "60s"),
            "err_threshold": int(opts.get("cooldown_err_threshold", 3)),
            "err_cooldown": opts.get("cooldown_err_cooldown", "10m"),
        },
        "schedule": {
            "checkin_times": normalize_checkin_times(opts.get("checkin_times")),
            "keepalive_hours": normalize_hours(opts.get("keepalive_hours")),
        },
        "upstream": {"timeout_seconds": int(opts.get("upstream_timeout", 120))},
    }


def write_config(cfg):
    os.makedirs(os.path.dirname(CONFIG_FILE), exist_ok=True)
    tmp = CONFIG_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(cfg, f, indent=2, ensure_ascii=False)
    os.replace(tmp, CONFIG_FILE)


# ---------------------------------------------------------------------------
# serverd 子进程管理
# ---------------------------------------------------------------------------
class ServerMgr:
    def __init__(self):
        self.proc = None
        self.started = 0.0
        self.last_error = ""
        self.lock = threading.Lock()

    def start(self, opts):
        with self.lock:
            cfg = build_config(opts)
            write_config(cfg)
            if self.proc and self.proc.poll() is None:
                self._kill_locked()
            try:
                self.proc = subprocess.Popen(
                    [SRVD_BIN, "-config", CONFIG_FILE],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                )
                self.started = time.time()
                self.last_error = ""
                return True
            except Exception as e:
                self.last_error = str(e)
                self.proc = None
                return False

    def _kill_locked(self):
        if self.proc:
            try:
                self.proc.terminate()
                try:
                    self.proc.wait(timeout=5)
                except Exception:
                    self.proc.kill()
            except Exception:
                pass
        self.proc = None

    def kill(self):
        with self.lock:
            self._kill_locked()

    def restart(self, opts=None):
        if opts is None:
            opts = load_options()
        with self.lock:
            cfg = build_config(opts)
            write_config(cfg)
            self._kill_locked()
            try:
                self.proc = subprocess.Popen(
                    [SRVD_BIN, "-config", CONFIG_FILE],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                )
                self.started = time.time()
                self.last_error = ""
                return True
            except Exception as e:
                self.last_error = str(e)
                return False

    def running(self):
        with self.lock:
            return bool(self.proc) and self.proc.poll() is None

    def uptime(self):
        if not self.running():
            return 0
        return int(time.time() - self.started)


G = ServerMgr()


# ---------------------------------------------------------------------------
# 子进程调用（ctl / login / logintrae）
# ---------------------------------------------------------------------------
def _run(cmd, timeout=60):
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout.strip(), r.stderr.strip()
    except Exception as e:
        return -1, "", str(e)


def ctl(mode, platform="", uid=""):
    cmd = [CTL_BIN, "-config", CONFIG_FILE, "-mode", mode]
    if platform:
        cmd += ["-p", platform]
    if uid:
        cmd += ["-uid", uid]
    rc, out, err = _run(cmd)
    if rc != 0:
        return None, err or out or ("ctl exited %d" % rc)
    try:
        return json.loads(out), None
    except Exception:
        return None, "ctl 输出非 JSON: %s" % out[:200]


# ---------------------------------------------------------------------------
# 账号数据（经 ctl accounts）
# ---------------------------------------------------------------------------
def list_accounts():
    data, err = ctl("accounts")
    if err:
        return [], err
    return data or [], None


def overview_data():
    accounts, err = list_accounts()
    if err:
        accounts = []
    total = len(accounts)
    wb = [a for a in accounts if a["kind"] == "workbuddy"]
    tr = [a for a in accounts if a["kind"] == "traework"]
    qd = [a for a in accounts if a["kind"] == "qoder"]
    wb_credits = sum(a.get("credits", 0) for a in wb)
    tr_credits = sum(a.get("credits", 0) for a in tr)
    qd_credits = sum(a.get("credits", 0) for a in qd)
    wb_ok = sum(1 for a in wb if not a.get("disabled") and not a.get("cooling"))
    tr_ok = sum(1 for a in tr if not a.get("disabled") and not a.get("cooling"))
    qd_ok = sum(1 for a in qd if not a.get("disabled") and not a.get("cooling"))
    opts = load_options()
    return {
        "server_up": G.running(),
        "uptime": G.uptime(),
        "last_error": G.last_error,
        "region": opts.get("region", "cn"),
        "api_key_set": bool(opts.get("api_key", "")),
        "port": PORT,
        "internal_port": SRVD_PORT,
        "account_total": total,
        "wb_total": len(wb), "wb_ok": wb_ok, "wb_credits": wb_credits,
        "tr_total": len(tr), "tr_ok": tr_ok, "tr_credits": tr_credits,
        "qd_total": len(qd), "qd_ok": qd_ok, "qd_credits": qd_credits,
        "checkin_times": normalize_checkin_times(opts.get("checkin_times")),
        "keepalive_hours": normalize_hours(opts.get("keepalive_hours")),
        "time": now_str(),
    }


# ---------------------------------------------------------------------------
# 登录
# ---------------------------------------------------------------------------
def wb_login_url():
    rc, out, err = _run([LOGIN_BIN, "url"])
    if rc != 0:
        return None, err or "登录启动失败"
    return out, None


def wb_login_poll():
    rc, out, err = _run([LOGIN_BIN, "poll"])
    if rc != 0:
        return None, err or "登录未完成"
    try:
        return json.loads(out), None
    except Exception:
        return None, "登录结果解析失败"


def trae_login_url(callback_path):
    """生成 Trae 授权 URL，回调指向本面板自身的 /api/trae-cb 端点。"""
    cmd = [LOGINTRAE_BIN, "url", "-state", TRAE_STATE, "-callback", callback_path]
    rc, out, err = _run(cmd)
    if rc != 0:
        return None, err or out or "TraeWork 登录启动失败"
    return out, None


def trae_complete(raw_callback_url):
    """浏览器登录后回调到本面板，据此换 token 并取账号信息。"""
    cmd = [LOGINTRAE_BIN, "complete", "-state", TRAE_STATE, "-url", raw_callback_url]
    rc, out, err = _run(cmd, timeout=30)
    if rc != 0:
        return None, err or out or "TraeWork 登录完成失败"
    try:
        return json.loads(out), None
    except Exception:
        return None, "TraeWork 登录结果解析失败"


def trae_complete_refresh(refresh_token, host=""):
    """直接用 refreshToken 换 token（最稳，不依赖浏览器回调/127.0.0.1 监听）。"""
    cmd = [LOGINTRAE_BIN, "complete", "-refresh", refresh_token]
    if host:
        cmd += ["-host", host]
    rc, out, err = _run(cmd, timeout=30)
    if rc != 0:
        return None, err or out or "TraeWork refreshToken 登录失败"
    try:
        return json.loads(out), None
    except Exception:
        return None, "TraeWork 登录结果解析失败"


def trae_poll():
    """轮询 TraeWork 回调是否已到达，完成后换 token 并取账号信息。"""
    cmd = [LOGINTRAE_BIN, "poll", "-state", TRAE_STATE]
    rc, out, err = _run(cmd, timeout=30)
    if rc != 0:
        return None, err or out or "TraeWork 登录未完成"
    try:
        return json.loads(out), None
    except Exception:
        return None, "TraeWork 登录结果解析失败"


def save_wb_auth(data):
    uid = data.get("uid", "")
    if not uid:
        return False, "缺少 uid"
    doc = {
        "account": {
            "uid": uid,
            "enterpriseId": data.get("enterprise_id", ""),
            "nickname": data.get("nickname", ""),
        },
        "auth": {
            "accessToken": data.get("access_token", ""),
            "refreshToken": data.get("refresh_token", ""),
            "expiresAt": int(time.time()) + int(data.get("expires_in", 604800)),
            "domain": data.get("domain", ""),
        },
    }
    return _write_auth_file(os.path.join(AUTH_DIR, "workbuddy-%s.json" % uid), doc)


def save_trae_auth(data):
    uid = data.get("uid", "")
    if not uid or not data.get("access_token"):
        return False, "TraeWork 登录结果缺少 uid 或 access_token"
    doc = {
        "account": {
            "uid": uid,
            "enterpriseId": data.get("enterprise_id", ""),
            "nickname": data.get("nickname", ""),
        },
        "auth": {
            "accessToken": data.get("access_token", ""),
            "refreshToken": data.get("refresh_token", ""),
            "expiresAt": int(data.get("expires_at") or 0),
            "domain": data.get("domain", "trae.cn"),
            "apiHost": data.get("api_host", ""),
            "machineId": data.get("machine_id", ""),
            "deviceId": data.get("device_id", ""),
        },
    }
    return _write_auth_file(os.path.join(AUTH_DIR, "trae-%s.json" % uid), doc)


def qoder_login_url():
    rc, out, err = _run([LOGINQODER_BIN, "url", "-authdir", AUTH_DIR, "-state", QODER_STATE])
    if rc != 0:
        return None, err or out or "Qoder 登录启动失败"
    return out, None


def qoder_login_poll():
    rc, out, err = _run([LOGINQODER_BIN, "poll", "-authdir", AUTH_DIR, "-state", QODER_STATE])
    if rc != 0:
        return None, err or out or "Qoder 登录未完成"
    try:
        return json.loads(out), None
    except Exception:
        return None, "Qoder 登录结果解析失败"


def _write_auth_file(fp, doc):
    os.makedirs(AUTH_DIR, exist_ok=True)
    try:
        tmp = fp + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(doc, f, indent=1, ensure_ascii=False)
        os.replace(tmp, fp)
        os.chmod(fp, 0o600)
        return True, "已保存: %s" % os.path.basename(fp)
    except Exception as e:
        return False, str(e)


def delete_account(kind, uid):
    if kind == "traework":
        prefix = "trae-"
    elif kind == "qoder":
        prefix = "qoder-"
    else:
        prefix = "workbuddy-"
    removed = False
    if os.path.isdir(AUTH_DIR):
        for fn in os.listdir(AUTH_DIR):
            if fn.startswith(prefix) and fn.endswith(".json"):
                fp = os.path.join(AUTH_DIR, fn)
                try:
                    with open(fp, "r", encoding="utf-8") as f:
                        raw = json.load(f)
                except Exception:
                    continue
                a = raw.get("account", {})
                if a.get("uid") == uid or uid and uid in fn:
                    os.remove(fp)
                    removed = True
                    break
    if removed:
        G.restart()
        return True, "账号已删除（后台已重启刷新账号池）"
    return False, "未找到该账号"


# ---------------------------------------------------------------------------
# HTTP 服务器
# ---------------------------------------------------------------------------
def match_key(path, key):
    return path.endswith("/api/" + key) or path.endswith("/wb-api/" + key)


def read_body(self):
    length = int(self.headers.get("Content-Length", 0))
    if length <= 0:
        return {}
    try:
        return json.loads(self.rfile.read(length).decode("utf-8", "replace"))
    except Exception:
        return {}


class LoginHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def _cookie(self):
        ck = self.headers.get("Cookie", "") or ""
        for part in ck.split(";"):
            part = part.strip()
            if part.startswith(WEBUI_COOKIE + "="):
                return part[len(WEBUI_COOKIE)+1:].strip()
        return ""

    def _handle_login_get(self):
        self._send(LOGIN_PAGE, "text/html; charset=utf-8")

    def _handle_login_post(self):
        body = read_body(self) or {}
        user = (body.get("user") or "").strip()
        pw = (body.get("pass") or "").strip()
        o = load_options()
        want_user = (o.get("webui_user") or "").strip()
        want_pass = (o.get("webui_pass") or "").strip()
        if want_user and user == want_user and pw == want_pass:
            tok = _webui_new_session()
            self._send_json({"ok": True, "token": tok})
        else:
            self._send_json({"error": "用户名或密码错误"}, 401)

    def _handle_qr(self):
        """本地二维码：后端代拉 qrserver，返回 PNG（浏览器无需直连外部）。"""
        q = {}
        try:
            q.update(urllib.parse.parse_qsl(urllib.parse.urlparse(self.path).query))
        except Exception:
            pass
        data = q.get("data", "")
        if not data:
            self._send("no data", "text/plain", 400)
            return
        url = "https://api.qrserver.com/v1/create-qr-code/?size=240x240&data=" + urllib.parse.quote(data, safe="")
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
            resp = urllib.request.urlopen(req, timeout=15)
            png = resp.read()
            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(png)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(png)
        except Exception as e:
            self._send("qr error: %s" % e, "text/plain", 502)

    def _send(self, data, ctype, code=200, extra=None):
        if isinstance(data, str):
            data = data.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)

    def _send_json(self, obj, code=200):
        self._send(json.dumps(obj, ensure_ascii=False), "application/json; charset=utf-8", code)

    # ---- 代理 /v1/* 到 serverd（流式透传） ----
    def _proxy(self, method):
        path = self.path
        # addon 安全：若配置了 api_key，/v1/* 必须带匹配的 Bearer
        o = load_options()
        want_key = (o.get("api_key") or "").strip()
        if want_key:
            auth = self.headers.get("Authorization", "") or ""
            got = auth[len("Bearer "):].strip() if auth.lower().startswith("bearer ") else ""
            if got != want_key:
                self._send_json({"error": {"code": "invalid_api_key", "message": "missing or invalid API key", "type": "api_error"}}, 401)
                return
        # 前端 wb-api/<key> 的管理接口在匹配到模型/状态时需重写为 serverd 的 /v1/* 路径
        if path.endswith("/wb-api/models") or path.endswith("/api/models"):
            path = "/v1/models"
        elif path.endswith("/wb-api/status") or path.endswith("/api/status"):
            path = "/status"
        elif path.endswith("/wb-api/healthz") or path.endswith("/api/healthz"):
            path = "/healthz"
        url = SRVD_UPSTREAM + path
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "connection")}
        body = None
        if method == "POST":
            body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0))
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            resp = urllib.request.urlopen(req, timeout=300)
            ctype = resp.headers.get("Content-Type", "application/octet-stream")
            self.send_response(resp.status)
            self.send_header("Content-Type", ctype)
            # 不设 Content-Length，按块透传并关闭连接结束，保证 SSE 流式逐 token 下发
            self.send_header("Connection", "close")
            self.end_headers()
            while True:
                chunk = resp.read(8192)
                if not chunk:
                    break
                try:
                    self.wfile.write(chunk)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    break
        except urllib.error.HTTPError as e:
            raw = e.read()
            ctype = e.headers.get("Content-Type", "application/octet-stream")
            self.send_response(e.code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                pass
        except Exception as e:
            self._send_json({"error": {"message": "upstream error: %s" % e, "type": "api_error", "code": "upstream_unavailable"}}, 502)

    # ---- handlers ----
    def _handle_fees(self):
        """转发费率查询到 serverd 的 /api/fees。"""
        try:
            req = urllib.request.Request(SRVD_UPSTREAM + "/api/fees", method="GET")
            with urllib.request.urlopen(req, timeout=20) as resp:
                data = json.loads(resp.read().decode("utf-8", "replace"))
            self._send_json(data)
        except urllib.error.HTTPError as e:
            body = ""
            try:
                body = e.read().decode("utf-8", "replace")[:300]
            except Exception:
                pass
            self._send_json({"error": "费率接口 HTTP %d: %s" % (e.code, body), "channels": []}, 502)
        except Exception as e:
            self._send_json({"error": "费率接口不可用：%s" % e, "channels": []}, 502)

    def _handle_fees_refresh(self):
        """请求 serverd 重新拉取费率，并立即返回当前缓存。"""
        try:
            req = urllib.request.Request(SRVD_UPSTREAM + "/api/fees/refresh", method="POST",
                                         data=b"{}", headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=30) as resp:
                data = json.loads(resp.read().decode("utf-8", "replace"))
            self._send_json(data)
        except urllib.error.HTTPError as e:
            body = ""
            try:
                body = e.read().decode("utf-8", "replace")[:300]
            except Exception:
                pass
            self._send_json({"error": "刷新费率 HTTP %d: %s" % (e.code, body), "channels": []}, 502)
        except Exception as e:
            self._send_json({"error": "刷新费率失败：%s" % e, "channels": []}, 502)

    def _handle_overview(self):
        self._send_json(overview_data())

    def _handle_accounts(self):
        accounts, err = list_accounts()
        if err:
            self._send_json({"error": err, "accounts": []}, 500)
            return
        self._send_json({"accounts": accounts})

    def _handle_models(self):
        # 后端注入 API Key 后再代理到 serverd /v1/models（避免前端无鉴权导致 401）
        opts = load_options()
        api_key = opts.get("api_key") or ""
        url = SRVD_UPSTREAM + "/v1/models"
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "connection")}
        if api_key:
            headers["Authorization"] = "Bearer " + api_key
        try:
            req = urllib.request.Request(url, headers=headers, method="GET")
            resp = urllib.request.urlopen(req, timeout=30)
            raw = resp.read().decode("utf-8", "replace")
            self._send_json(json.loads(raw) if raw.strip() else {"data": []})
        except urllib.error.HTTPError as e:
            raw = (e.read() or b"")[:300].decode("utf-8", "replace")
            self._send_json({"error": "模型接口不可用 (HTTP %d): %s" % (e.code, raw)}, 502)
        except Exception as e:
            self._send_json({"error": "模型接口不可用: %s" % e}, 502)

    def _handle_config_get(self):
        self._send_json({"options": load_options(), "config_file": CONFIG_FILE})

    def _handle_logout(self):
        """退出登录：作废当前会话 token 并提示浏览器清除 cookie。"""
        try:
            if os.path.exists(SESSION_FILE):
                os.remove(SESSION_FILE)
        except Exception:
            pass
        self._send_json({"success": True, "message": "已退出登录", "clear_cookie": WEBUI_COOKIE})

    def _handle_change_login(self):
        """修改面板登录账号/密码（需已登录会话）。"""
        if not _webui_check_cookie(self._cookie()):
            self._send_json({"error": "未登录"}, 401)
            return
        body = read_body(self) or {}
        new_user = (body.get("user") or "").strip()
        new_pass = (body.get("pass") or "").strip()
        if not new_user:
            self._send_json({"error": "登录名不能为空"}, 400)
            return
        o = load_options()
        if new_pass:
            o["webui_user"] = new_user
            o["webui_pass"] = new_pass
        else:
            o["webui_user"] = new_user  # 仅改登录名，密码保持
        save_options(o)
        self._send_json({"success": True, "message": "登录账号已更新，下次登录生效"})

    def _handle_config_save(self):
        body = read_body(self)
        incoming = body.get("options") or {}
        opts = load_options()
        for k in DEFAULT_OPTIONS:
            if k in incoming:
                opts[k] = incoming[k]
        opts["checkin_times"] = normalize_checkin_times(opts["checkin_times"])
        opts["keepalive_hours"] = normalize_hours(opts["keepalive_hours"])
        try:
            opts["cooldown_err_threshold"] = int(opts.get("cooldown_err_threshold", 3))
            opts["upstream_timeout"] = int(opts.get("upstream_timeout", 120))
        except Exception:
            pass
        save_options(opts)
        ok = G.restart(opts)
        if not ok:
            self._send_json({"error": "serverd 重启失败: %s" % G.last_error}, 500)
            return
        self._send_json({"success": True, "message": "配置已保存，serverd 已在后台重启生效"})

    # ---- 登录 ----
    def _handle_wb_url(self):
        url, err = wb_login_url()
        if err:
            self._send_json({"error": err}, 500)
        else:
            self._send_json({"url": url})

    def _handle_wb_poll(self):
        data, err = wb_login_poll()
        if err:
            self._send_json({"error": err}, 500)
            return
        ok, msg = save_wb_auth(data)
        if ok:
            G.restart()
            self._send_json({"success": True, "message": msg + "（后台重启以纳入账号）"})
        else:
            self._send_json({"error": msg}, 500)

    def _handle_trae_url(self):
        host = self.headers.get("Host", "") or "127.0.0.1"
        # 判断 scheme：优先信任反代透传的 X-Forwarded-Proto/Scheme；否则 HTTPS 端口(443/8443/4438/7443)用 https
        scheme = "http"
        fwd = self.headers.get("X-Forwarded-Proto") or self.headers.get("X-Forwarded-Scheme", "")
        if fwd in ("https", "http"):
            scheme = fwd
        else:
            port = ""
            if host.startswith("["):  # IPv6 like [::1]:8443
                port = host.rsplit(":", 1)[-1] if "]" in host and ":" in host.split("]")[1] else ""
            elif ":" in host:
                port = host.rsplit(":", 1)[-1]
            if port in ("443", "8443", "4438", "7443", "4443"):
                scheme = "https"
        # 保留 ingress 前缀路径（如 /api/hassio_ingress/<token>/），确保回调能回到面板而非 HA 入口
        try:
            p = urllib.parse.urlparse(self.path).path
            if p.endswith("/api/trae-url"):
                prefix = p[: -len("/api/trae-url")]
            elif p.endswith("/wb-api/trae-url"):
                prefix = p[: -len("/wb-api/trae-url")]
            else:
                prefix = p.rstrip("/")
            callback = scheme + "://" + host + prefix.rstrip("/") + "/api/trae-cb"
        except Exception:
            callback = scheme + "://" + host + "/api/trae-cb"
        url, err = trae_login_url(callback)
        if err:
            self._send_json({"error": err}, 500)
        else:
            self._send_json({"url": url, "callback": callback})

    def _handle_trae_cb(self):
        """Trae 授权页登录后重定向/回调到本端点，据此完成登录并写入账号。"""
        q = {}
        try:
            q.update(urllib.parse.parse_qsl(urllib.parse.urlparse(self.path).query))
        except Exception:
            pass
        if self.command == "POST":
            body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0)).decode("utf-8", "replace")
            try:
                bd = json.loads(body)
                if isinstance(bd, dict):
                    for k, v in bd.items():
                        if isinstance(v, str) and k not in q:
                            q[k] = v
            except Exception:
                try:
                    for k, v in urllib.parse.parse_qsl(body):
                        q.setdefault(k, v)
                except Exception:
                    pass
        host = self.headers.get("Host", "") or "127.0.0.1"
        raw = "http://%s/api/trae-cb?%s" % (host, urllib.parse.urlencode(q))
        data, err = trae_complete(raw)
        if err:
            self._send('<html><body style="font-family:sans-serif;padding:24px">TraeWork 登录失败：<br>%s<br><a href="/" target="_blank">返回面板</a></body></html>' % err, "text/html; charset=utf-8", 500)
            return
        ok, msg = save_trae_auth(data)
        if ok:
            G.restart()
        self._send('<html><body style="font-family:sans-serif;padding:24px">%s<br>可以关闭此页面/返回面板查看账号。</body></html>' % msg, "text/html; charset=utf-8")

    def _handle_trae_complete(self):
        """方案B：用户粘贴授权页跳转后的回调 URL，据此完成换 token 并写入账号。"""
        body = read_body(self) or {}
        raw_url = (body.get("url") or "").strip()
        if not raw_url:
            # 兼容 GET query 方式
            try:
                raw_url = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query).get("url", [""])[0]
            except Exception:
                raw_url = ""
        if not raw_url.startswith("http"):
            self._send_json({"error": "缺少或非法的授权回调 URL"}, 400)
            return
        data, err = trae_complete(raw_url)
        if err:
            self._send_json({"error": err}, 500)
            return
        ok, msg = save_trae_auth(data)
        if ok:
            G.restart()
            self._send_json({"success": True, "message": msg + "（后台已重启纳入账号）"})
        else:
            self._send_json({"error": msg}, 500)

    def _handle_trae_complete_refresh(self):
        """直接用 refreshToken 换 token 并写入账号（最稳，不依赖浏览器回调）。"""
        body = read_body(self) or {}
        rt = (body.get("refresh_token") or "").strip()
        if not rt:
            self._send_json({"error": "缺少 refreshToken"}, 400)
            return
        data, err = trae_complete_refresh(rt)
        if err:
            self._send_json({"error": err}, 500)
            return
        ok, msg = save_trae_auth(data)
        if ok:
            G.restart()
            self._send_json({"success": True, "message": msg + "（后台已重启纳入账号）"})
        else:
            self._send_json({"error": msg}, 500)

    def _handle_trae_poll(self):
        """前端手动确认 TraeWork 登录已完成，轮询 state 并完成换 token。"""
        data, err = trae_poll()
        if err:
            self._send_json({"error": err}, 500)
            return
        ok, msg = save_trae_auth(data)
        if ok:
            G.restart()
            self._send_json({"success": True, "message": msg + "（后台已重启纳入账号）"})
        else:
            self._send_json({"error": msg}, 500)

    # ---- Qoder 登录（PKCE 设备流：url 取授权链接，poll 完成） ----
    def _handle_qoder_url(self):
        url, err = qoder_login_url()
        if err:
            self._send_json({"error": err}, 500)
        else:
            self._send_json({"url": url})

    def _handle_qoder_poll(self):
        data, err = qoder_login_poll()
        if err:
            self._send_json({"error": err}, 500)
            return
        uid = data.get("uid", "")
        if not uid:
            self._send_json({"error": "Qoder 登录结果缺少 uid"}, 500)
            return
        ok, msg = True, "Qoder 凭证已保存（loginqoder 已写 auth 文件）"
        if ok:
            G.restart()
            self._send_json({"success": True, "message": msg + "（后台已重启纳入账号）"})

    # ---- 账号动作 ----
    def _handle_action(self, action):
        body = read_body(self)
        platform = body.get("platform") or body.get("p") or ""
        uid = body.get("uid") or ""
        data, err = ctl(action, platform, uid)
        if err:
            self._send_json({"error": err, "results": []}, 500)
            return
        if not isinstance(data, list):
            self._send_json({"success": True, "results": [data]})
            return
        self._send_json({"success": True, "message": "已处理 %d 个账号" % len(data), "results": data})

    # ---- delete ----
    def _handle_delete(self):
        body = read_body(self)
        kind = body.get("platform") or body.get("kind") or "workbuddy"
        uid = body.get("uid") or ""
        if not uid:
            self._send_json({"error": "缺少 uid"}, 400)
            return
        ok, msg = delete_account(kind, uid)
        if not ok:
            self._send_json({"error": msg}, 404)
        else:
            self._send_json({"success": True, "message": msg})

    # ---- 路由 ----
    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        if path == "/healthz":
            self._send("OK", "text/plain")
        # ---- Web UI 面板登录门禁 ----
        # ingress 访问（如 /api/hassio_ingress/<token>/...）按面板首页处理：未登录返回登录页，不放行 401
        is_ingress = path.startswith("/api/hassio_ingress/") or "/hassio_ingress/" in path
        if path == "/" or path.endswith("/") or is_ingress:
            if _webui_enabled() and not _webui_check_cookie(self._cookie()):
                self._send(LOGIN_PAGE, "text/html; charset=utf-8")
                return
            self._send(HTML_PAGE, "text/html; charset=utf-8")
        elif match_key(path, "login"):
            self._handle_login_get()
        elif match_key(path, "qr"):
            self._handle_qr()
        elif _webui_enabled() and not _webui_check_cookie(self._cookie()) and not path.startswith(PROXY_PREFIX):
            # 面板管理接口需登录；/v1/* API 走 api_key，不要求面板登录
            self._send_json({"error": "未登录，请先访问面板首页登录"}, 401)
        elif match_key(path, "overview"):
            self._handle_overview()
        elif match_key(path, "accounts"):
            self._handle_accounts()
        elif match_key(path, "models"):
            self._handle_models()
        elif match_key(path, "config"):
            self._handle_config_get()
        elif match_key(path, "wb-url"):
            self._handle_wb_url()
        elif match_key(path, "trae-url"):
            self._handle_trae_url()
        elif match_key(path, "trae-cb"):
            self._handle_trae_cb()
        elif match_key(path, "trae-complete"):
            self._handle_trae_complete()
        elif match_key(path, "trae-complete-refresh"):
            self._handle_trae_complete_refresh()
        elif match_key(path, "trae-poll"):
            self._handle_trae_poll()
        elif match_key(path, "qoder-url"):
            self._handle_qoder_url()
        elif path.startswith(PROXY_PREFIX) or path == "/status":
            self._proxy("GET")
        else:
            self._send("404 Not Found", "text/plain", 404)

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        if match_key(path, "login"):
            self._handle_login_post()
        elif match_key(path, "logout"):
            self._handle_logout()
        elif match_key(path, "change-login"):
            self._handle_change_login()
        elif match_key(path, "config"):
            self._handle_config_save()
        elif match_key(path, "wb-poll"):
            self._handle_wb_poll()
        elif match_key(path, "trae-cb"):
            self._handle_trae_cb()
        elif match_key(path, "trae-complete"):
            self._handle_trae_complete()
        elif match_key(path, "trae-complete-refresh"):
            self._handle_trae_complete_refresh()
        elif match_key(path, "trae-poll"):
            self._handle_trae_poll()
        elif match_key(path, "qoder-poll"):
            self._handle_qoder_poll()
        elif match_key(path, "fees-refresh"):
            self._handle_fees_refresh()
        elif match_key(path, "fees"):
            self._handle_fees()
        elif match_key(path, "credits"):
            self._handle_action("credits")
        elif match_key(path, "checkin"):
            self._handle_action("checkin")
        elif match_key(path, "refresh"):
            self._handle_action("refresh")
        elif match_key(path, "delete"):
            self._handle_delete()
        elif path.startswith(PROXY_PREFIX):
            self._proxy("POST")
        else:
            self._send("404 Not Found", "text/plain", 404)


# ---------------------------------------------------------------------------
# HTML 页面
# ---------------------------------------------------------------------------
LOGIN_PAGE = r"""<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>AI Proxy 登录</title>
<style>
body{margin:0;background:#0f1115;color:#e6e9ef;font:14px/1.6 -apple-system,"Segoe UI",Roboto,"Microsoft YaHei",sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh}
.card{background:#171a21;border:1px solid #2a2f3a;border-radius:12px;padding:28px;width:320px;max-width:92vw}
h1{font-size:18px;margin:0 0 18px}
label{display:block;margin:10px 0 4px;color:#9aa3b2;font-size:13px}
input{width:100%;padding:10px 12px;border-radius:8px;border:1px solid #2a2f3a;background:#0c0e12;color:#e6e9ef;font-size:14px;box-sizing:border-box}
button{margin-top:18px;width:100%;padding:11px;border:0;border-radius:8px;background:#0a84ff;color:#fff;font-size:14px;cursor:pointer}
button:hover{background:#3395ff}
.err{color:#ff453a;font-size:13px;margin-top:10px;min-height:16px}
</style></head><body>
<div class="card">
<h1>AI Proxy 管理面板</h1>
<form id="lf">
<label>账号</label><input id="lu" autocomplete="username">
<label>密码</label><input id="lp" type="password" autocomplete="current-password">
<button type="submit">登录</button>
</form>
<div class="err" id="lerr"></div>
</div>
<script>
document.getElementById('lf').addEventListener('submit',async function(ev){ev.preventDefault();
var u=document.getElementById('lu').value,p=document.getElementById('lp').value,err=document.getElementById('lerr');err.textContent='';
try{var r=await fetch('wb-api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:u,pass:p})});
var d=await r.json();if(d.ok){document.cookie='ai_proxy_session='+d.token+';path=/';location.reload();}else{err.textContent=d.error||'登录失败';}}
catch(e){err.textContent='网络错误: '+e.message;}});
</script>
</body></html>"""

# ---------------------------------------------------------------------------
HTML_PAGE = r"""<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>AI Proxy 管理</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--card2:#1d2129;--line:#2a2f3a;--txt:#e6e9ef;--sub:#9aa3b2;--pri:#0a84ff;--pri2:#3395ff;--ok:#32d74b;--warn:#ff9f0a;--err:#ff453a;--info:#a0d7ff;}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--txt);font:14px/1.6 -apple-system,"Segoe UI",Roboto,"Microsoft YaHei",sans-serif}
.wrap{max-width:1120px;margin:0 auto;padding:20px}
.top{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:16px}
.brand{display:flex;align-items:center;gap:12px}
.brand h1{font-size:20px;margin:0}
.brand .sub{font-size:12px;color:var(--sub)}
.hd-right{display:flex;align-items:center;gap:10px}
.pill{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--sub);border:1px solid var(--line);border-radius:20px;padding:4px 12px}
.dot{width:8px;height:8px;border-radius:50%;background:#555}
.dot.up{background:var(--ok)}.dot.down{background:var(--err)}
.tabs{display:flex;gap:4px;border-bottom:1px solid var(--line);margin-bottom:16px;flex-wrap:wrap}
.tab{background:transparent;border:1px solid transparent;color:var(--sub);padding:9px 16px;font-size:14px;cursor:pointer;border-radius:8px 8px 0 0;display:inline-flex;gap:6px;align-items:center}
.tab:hover{color:var(--txt)}
.tab.active{color:var(--pri);border-color:var(--line);border-bottom-color:var(--pri);background:rgba(10,132,255,.06)}
.panel{display:none}.panel.active{display:block}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:12px;margin-bottom:16px}
.stat{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px}
.stat .lbl{font-size:12px;color:var(--sub);margin-bottom:6px}
.stat .val{font-size:20px;font-weight:700}
.val.good{color:var(--ok)}.val.warn{color:var(--warn)}.val.bad{color:var(--err)}
.box{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px;margin-bottom:16px}
.box h2{font-size:15px;margin:0 0 12px;display:flex;align-items:center;gap:8px}
.hint{font-size:12px;color:var(--sub);line-height:1.6}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--line)}
th{color:var(--sub);font-weight:500;font-size:12px;white-space:nowrap;background:var(--card2)}
tr:hover td{background:rgba(255,255,255,.02)}
.subhead{font-size:13px;color:var(--pri);margin:18px 0 8px;font-weight:600}
.badge{display:inline-flex;align-items:center;gap:5px;font-size:11px;padding:2px 8px;border-radius:6px;font-weight:500}
.b-ok{background:rgba(50,215,75,.15);color:var(--ok)}
.b-warn{background:rgba(255,159,10,.15);color:var(--warn)}
.b-bad{background:rgba(255,69,58,.15);color:var(--err)}
.b-info{background:rgba(160,215,255,.25);color:#0a84ff}
.rowbtns{display:flex;gap:5px;flex-wrap:wrap}
.btn{display:inline-flex;align-items:center;gap:5px;border:none;border-radius:7px;padding:6px 10px;font-size:12px;cursor:pointer;color:#fff}
.btn.sm{padding:4px 8px;font-size:11px}
.btn-pri{background:var(--pri)}.btn-pri:hover{background:var(--pri2)}
.btn-ok{background:var(--ok);color:#06210b}.btn-ok:hover{filter:brightness(1.1)}
.btn-warn{background:var(--warn)}.btn-warn:hover{filter:brightness(1.1)}
.btn-info{background:var(--info);color:#0f1115}.btn-info:hover{filter:brightness(1.1)}
.btn-danger{background:var(--err)}.btn-danger:hover{filter:brightness(1.1)}
.btn:disabled{opacity:.5;cursor:not-allowed}
.tbar{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:12px;flex-wrap:wrap}
.tbar .grp{display:flex;gap:8px;flex-wrap:wrap}
.field{margin-bottom:12px}
.field label{display:block;font-size:12px;color:var(--sub);margin-bottom:5px}
.field input,.field select{width:100%;background:var(--card2);border:1px solid var(--line);color:var(--txt);border-radius:7px;padding:8px 10px;font-size:13px}
.fsec{margin-bottom:18px}
.fsec h3{font-size:13px;color:var(--sub);margin:0 0 8px;border-bottom:1px solid var(--line);padding-bottom:6px}
.login-box{background:var(--card2);border:1px dashed var(--line);border-radius:10px;padding:14px;margin-bottom:12px}
.qr{background:#fff;border-radius:10px;padding:12px;display:inline-block;text-align:center}
.qr img{max-width:200px;border-radius:6px}
.qr .ph{color:#999;font-size:13px}
.empty{color:var(--sub);text-align:center;padding:24px;font-size:13px}
.toast{position:fixed;top:16px;right:16px;z-index:999;display:flex;flex-direction:column;gap:8px}
.toast .t{min-width:260px;max-width:360px;padding:12px 14px;border-radius:9px;font-size:13px;background:var(--card2);border:1px solid var(--line);box-shadow:0 8px 24px rgba(0,0,0,.4);line-height:1.5}
.toast .t.ok{border-color:var(--ok)}.toast .t.err{border-color:var(--err)}
</style></head><body><div class="wrap">
<header class="top"><div class="brand">
<svg width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="#0a84ff" stroke-width="2"><rect x="4" y="4" width="7" height="7" rx="1"/><rect x="13" y="4" width="7" height="7" rx="1"/><rect x="4" y="13" width="7" height="7" rx="1"/><rect x="13" y="13" width="7" height="7" rx="1"/></svg>
<div><h1>AI Proxy</h1><div class="sub">WorkBuddy + TraeWork 多平台 AI 代理</div></div></div>
<div class="hd-right"><span class="pill"><span class="dot" id="srvDot"></span><span id="srvTxt">检测中…</span></span>
<span class="pill" id="loginUser" style="display:none"></span>
<button class="btn btn-pri" onclick="refreshAll()">刷新</button>
<button class="btn btn-warn" id="btnLogout" onclick="doLogout()" style="display:none">退出登录</button></div></header>
<nav class="tabs">
<button class="tab active" data-p="overview" onclick="switchPanel('overview')">概览</button>
<button class="tab" data-p="accounts" onclick="switchPanel('accounts')">账号</button>
<button class="tab" data-p="models" onclick="switchPanel('models')">模型</button>
<button class="tab" data-p="fees" onclick="switchPanel('fees')">费率</button>
<button class="tab" data-p="settings" onclick="switchPanel('settings')">设置</button>
</nav>
<div class="toast" id="toastBox"></div>

<section class="panel active" id="panel-overview">
 <div class="cards" id="ovCards"></div>
 <div class="box"><h2>服务状态</h2><div class="hint" id="ovDetail"></div></div>
</section>

<section class="panel" id="panel-accounts">
 <div class="login-box">
   <b>添加账号</b>
   <div class="rowbtns" style="margin-top:8px">
     <button class="btn btn-pri" onclick="wbLogin()">WorkBuddy 扫码登录</button>
     <button class="btn btn-info" onclick="traeLogin()">TraeWork 登录</button>
     <button class="btn btn-warn" onclick="qoderLogin()">Qoder 登录</button>
   </div>
   <div class="hint" id="loginHint" style="margin-top:8px"></div>
   <div id="loginShow"></div>
 </div>
 <div class="tbar"><div class="grp">
   <button class="btn btn-warn" onclick="runAll('checkin','')">全部签到</button>
   <button class="btn btn-pri" onclick="runAll('credits','')">全部刷新积分</button>
   <button class="btn btn-info" onclick="runAll('refresh','')">全部刷新Token</button>
 </div><span class="hint" id="acctCount">账号：加载中…</span></div>
 <div class="box">
   <div class="subhead">WorkBuddy（CodeBuddy）</div>
   <table><thead><tr><th>昵称</th><th>UID</th><th>积分</th><th>状态</th><th>Token</th><th>操作</th></tr></thead>
   <tbody id="wbBody"></tbody></table>
   <div class="subhead">TraeWork</div>
   <table><thead><tr><th>昵称</th><th>UID</th><th>积分</th><th>状态</th><th>Token</th><th>操作</th></tr></thead>
   <tbody id="trBody"></tbody></table>
   <div class="subhead">Qoder</div>
   <table><thead><tr><th>昵称</th><th>UID</th><th>积分</th><th>状态</th><th>Token</th><th>操作</th></tr></thead>
   <tbody id="qdBody"></tbody></table>
   <div class="empty" id="acctEmpty" style="display:none">暂无账号，请用上方按钮添加。</div>
 </div>
</section>

<section class="panel" id="panel-models">
 <div class="tbar"><div class="grp"><button class="btn btn-pri" onclick="loadModels()">刷新模型</button></div>
 <span class="hint" id="modelInfo"></span></div>
 <div class="box">
  <p class="hint" style="margin:0 0 8px">模型带来源前缀：<code>workbuddy/&lt;model&gt;</code>、<code>traework/&lt;model&gt;</code> 或 <code>qoder/&lt;model&gt;</code>。客户端调用时必须带前缀。</p>
  <table><thead><tr><th>模型 ID</th><th>上下文(tokens)</th><th>最大输出(tokens)</th></tr></thead><tbody id="modelBody"></tbody></table>
  <div class="empty" id="modelEmpty" style="display:none">未加载到模型列表。</div>
 </div>
</section>

<section class="panel" id="panel-fees">
 <div class="tbar">
  <div class="grp"><button class="btn btn-pri" id="btnRefreshFees" onclick="refreshFees()">刷新费率</button></div>
  <span class="hint" id="feesInfo"></span>
 </div>
 <div class="box" id="feesBox">
  <p class="hint" style="margin:0">加载中…</p>
 </div>
</section>

<section class="panel" id="panel-settings">
 <div class="box"><h2>设置</h2>
  <p class="hint" style="margin:0 0 14px">保存后写入 <code>/data/options.json</code> 并热重启 serverd 生效（不影响本面板）。</p>
  <div class="fsec"><h3>通用</h3>
    <div class="field"><label>API Key（留空则不鉴权）</label><input id="f_api_key" placeholder="OpenAI 客户端调用所需 Key"></div>
    <div class="field"><label>WorkBuddy 注册区域</label><select id="f_region"><option value="cn">cn</option><option value="global">global</option></select></div>
    <div class="field"><label>上游超时（秒）</label><input id="f_upstream_timeout" type="number" min="1" max="600"></div>
  </div>
  <div class="fsec"><h3>冷却策略</h3>
    <div class="field"><label>余额不足冷却时长</label><input id="f_cooldown_hard_credit" placeholder="如 12h"></div>
    <div class="field"><label>限流(429)冷却时长</label><input id="f_cooldown_soft_rate" placeholder="如 60s"></div>
    <div class="field"><label>连续错误触发阈值</label><input id="f_cooldown_err_threshold" type="number" min="1"></div>
    <div class="field"><label>连续错误冷却时长</label><input id="f_cooldown_err_cooldown" placeholder="如 10m"></div>
  </div>
  <div class="fsec"><h3>面板登录账号</h3>
    <div class="field"><label>登录名</label><input id="f_webui_user" placeholder="面板登录账号"></div>
    <div class="field"><label>新密码</label><input id="f_webui_pass" type="password" placeholder="面板登录密码（留空则不修改密码）"></div>
    <div class="tbar"><button class="btn btn-warn" onclick="changeLogin()">保存登录账号</button></div>
    <p class="hint" style="margin:8px 0 0">修改后立即生效；当前会话保持，下次登录用新账号。</p>
  </div>
  <div class="fsec"><h3>定时任务</h3>
    <div class="field"><label>每日签到时刻（HH:MM，逗号分隔，两个平台共用）</label><input id="f_checkin_times" placeholder="如 09:00,21:00"></div>
    <div class="field"><label>Token 保活时刻（整点小时，逗号分隔）</label><input id="f_keepalive_hours" placeholder="如 22"></div>
  </div>
  <div class="tbar"><button class="btn btn-ok" onclick="saveSettings()">保存设置</button>
  <button class="btn btn-pri" onclick="loadSettings()">重新加载</button></div>
 </div>
</section>
</div>
<script>
let ovTimer=null;
function toast(msg,type){const b=document.getElementById('toastBox');const t=document.createElement('div');t.className='t '+(type||'info');t.textContent=msg;b.appendChild(t);setTimeout(()=>{t.style.opacity=0;t.style.transition='opacity .3s';setTimeout(()=>t.remove(),300);},4200);}
async function api(key,opts){opts=opts||{};try{const r=await fetch('wb-api/'+key,{method:opts.method||'GET',headers:opts.body?{'Content-Type':'application/json'}:{},body:opts.body?JSON.stringify(opts.body):undefined});const txt=await r.text();let d;try{d=JSON.parse(txt);}catch(e){d={error:txt,status:r.status};}return d;}catch(e){return{error:'网络错误: '+e.message};}}
function esc(s){s=(s===null||s===undefined)?'':String(s);return s.replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));}
async function loadFees(){
  const box=document.getElementById('feesBox');
  if(!box) return;
  const d=await api('fees');
  renderFees(d);
}

async function refreshFees(){
  const btn=document.getElementById('btnRefreshFees');
  if(btn) btn.disabled=true;
  try{
    const d=await api('fees-refresh',{method:'POST'});
    renderFees(d);
    toast('费率已刷新（后台重新拉取，稍后自动更新）','ok');
    setTimeout(loadFees,3000);
  }catch(e){ toast((e&&e.message)||'刷新失败','err'); }
  finally{ if(btn) btn.disabled=false; }
}

function renderFees(fees){
  const box=document.getElementById('feesBox');
  if(!box) return;
  const channels=(fees&&fees.channels)||[];
  let html='';
  if(fees&&fees.note) html+='<p class="hint" style="margin:0 0 6px">'+esc(fees.note)+'</p>';
  if(fees&&fees.cached_at) html+='<p class="hint" style="margin:0 0 6px">上次更新：'+esc(fees.cached_at)+'</p>';
  if(fees&&fees.error) html+='<p class="hint" style="margin:0 0 6px;color:var(--err)">'+esc(fees.error)+'</p>';
  if(channels.length===0){
    html+='<p class="hint" style="margin:0">'+esc((fees&&fees.disclaimer)||'')+'</p>';
    box.innerHTML=html; return;
  }
  html+='<table><thead><tr><th>模型</th><th>倍率</th><th>模型</th><th>倍率</th></tr></thead><tbody>';
  for(const ch of channels){
    const chName = ch.channel==='traework'?'TraeWork':(ch.channel==='qoder'?'Qoder':'WorkBuddy');
    html+='<tr><td colspan="4" style="color:var(--sub);font-weight:600">'+esc(chName)+'</td></tr>';
    const models=ch.models||[];
    for(let i=0;i<models.length;i+=2){
      const m1=models[i], m2=models[i+1];
      html+='<tr><td><code>'+esc(m1.model)+'</code></td><td>'+fmtRate(m1)+'</td>';
      html+='<td>'+((m2&&m2.model)?'<code>'+esc(m2.model)+'</code>':'')+'</td><td>'+((m2&&m2.model)?fmtRate(m2):'')+'</td></tr>';
    }
  }
  html+='</tbody></table>';
  if(fees&&fees.disclaimer) html+='<p class="hint" style="margin:8px 0 0">'+esc(fees.disclaimer)+'</p>';
  box.innerHTML=html;
}

function fmtRate(m){
  if(!m) return '';
  const r=(m.rate===undefined||m.rate===null)?0:m.rate;
  let s=(Number(r)||0).toString();
  if(m.note) s+=' <span style="color:var(--sub);font-size:11px">'+esc(m.note)+'</span>';
  return s;
}

function switchPanel(n){document.querySelectorAll('.tab').forEach(b=>b.classList.toggle('active',b.dataset.p===n));document.querySelectorAll('.panel').forEach(p=>p.classList.remove('active'));document.getElementById('panel-'+n).classList.add('active');if(n==='overview')loadOverview(true);if(n==='accounts')loadAccounts();if(n==='models')loadModels();if(n==='fees')loadFees();if(n==='settings')loadSettings();}
function refreshAll(){loadOverview(true);loadAccounts();}
function fmtT(v){if(!v)return '—';const t=new Date(v*1000);if(isNaN(t))return String(v);return t.toLocaleString('zh-CN',{hour12:false});}
function stateBadge(a){if(a.disabled)return '<span class="badge b-bad">已禁用</span>';if(a.cooling)return '<span class="badge b-warn">冷却中</span>'+(a.reason?'<div class="hint">'+esc(a.reason)+'</div>':'');return '<span class="badge b-ok">可用</span>';}
function tokenCell(a){let h='<span class="badge" style="background:rgba(255,255,255,.06);color:var(--sub)">无刷新令牌</span>';if(a.has_refresh){const left=(a.expires_at||0)-(Date.now()/1000);h=left<=0?'<span class="badge b-bad">已过期</span>':(left<86400?'<span class="badge b-warn">即将过期</span>':'<span class="badge b-info">正常</span>');}return fmtT(a.expires_at)+'<div class="hint">'+h+'</div>';}
async function loadOverview(force){const d=await api('overview');if(d.error){toast(d.error,'err');return;}const dot=document.getElementById('srvDot'),txt=document.getElementById('srvTxt');dot.className='dot '+(d.server_up?'up':'down');txt.textContent=d.server_up?'运行中':'已停止';
const cards=[
{l:'服务状态',v:d.server_up?'运行中':'已停止',c:d.server_up?'good':'bad'},
{l:'账号总数',v:d.account_total,c:''},
{l:'WorkBuddy',v:d.wb_total+' / 可用 '+d.wb_ok,c:d.wb_ok>0?'good':''},
{l:'TraeWork',v:d.tr_total+' / 可用 '+d.tr_ok,c:d.tr_ok>0?'good':''},
{l:'Qoder',v:d.qd_total+' / 可用 '+d.qd_ok,c:d.qd_ok>0?'good':''},
{l:'WB 积分',v:(d.wb_credits||0).toLocaleString(),c:'good'},
{l:'TR 积分',v:(d.tr_credits||0).toLocaleString(),c:'good'},
{l:'QD 积分',v:(d.qd_credits||0).toLocaleString(),c:'good'},
{l:'地区',v:esc(d.region),c:''},
{l:'API Key',v:d.api_key_set?'已设置':'未设置',c:d.api_key_set?'good':'warn'}];
document.getElementById('ovCards').innerHTML=cards.map(c=>'<div class="stat"><div class="lbl">'+c.l+'</div><div class="val '+c.c+'">'+c.v+'</div></div>').join('');
document.getElementById('ovDetail').innerHTML=('端口 '+d.port+'（公开/ingress） / '+d.internal_port+'（内部）｜运行时长 '+d.uptime+'s<br>签到时刻：'+(d.checkin_times||[]).join('、')+'｜Token 保活：'+(d.keepalive_hours||[]).join('、')+' 点'+(d.server_up?'':('<br>⚠ '+esc(d.last_error||'进程异常'))));
scheduleOv(!force);}
function scheduleOv(c){if(ovTimer)clearTimeout(ovTimer);if(c)ovTimer=setTimeout(()=>loadOverview(false),6000);}
async function loadAccounts(){const d=await api('accounts');const wbB=document.getElementById('wbBody'),trB=document.getElementById('trBody'),qdB=document.getElementById('qdBody'),empty=document.getElementById('acctEmpty'),cnt=document.getElementById('acctCount');if(d.error){toast(d.error,'err');return;}
const accs=d.accounts||[];const wb=accs.filter(a=>a.kind==='workbuddy'),tr=accs.filter(a=>a.kind==='traework'),qd=accs.filter(a=>a.kind==='qoder');
const isQ=a=>a.kind==='qoder';
const row=a=>'<tr><td><b>'+esc(a.nickname||'未命名')+'</b></td><td class="hint">'+esc(a.uid)+'</td><td><b>'+(a.credits||0).toLocaleString()+'</b></td><td>'+stateBadge(a)+'</td><td>'+tokenCell(a)+'</td><td><div class="rowbtns">'+
(isQ(a)?'':('<button class="btn sm btn-ok" onclick="acctAction(&#39;checkin&#39;,&#39;'+a.kind+'&#39;,&#39;'+esc(a.uid)+'&#39;)">签到</button>'))+
'<button class="btn sm btn-pri" onclick="acctAction(&#39;credits&#39;,&#39;'+a.kind+'&#39;,&#39;'+esc(a.uid)+'&#39;)">刷新积分</button>'+
'<button class="btn sm btn-info" onclick="acctAction(&#39;refresh&#39;,&#39;'+a.kind+'&#39;,&#39;'+esc(a.uid)+'&#39;)">刷新Token</button>'+
'<button class="btn sm btn-danger" onclick="delAcct(&#39;'+a.kind+'&#39;,&#39;'+esc(a.uid)+'&#39;)">删除</button></div></td></tr>';
wbB.innerHTML=wb.map(row).join('');trB.innerHTML=tr.map(row).join('');qdB.innerHTML=qd.map(row).join('');
empty.style.display=(wb.length+tr.length+qd.length)?'none':'block';
cnt.textContent='账号：WorkBuddy '+wb.length+' / TraeWork '+tr.length+' / Qoder '+qd.length+' 个';}
async function acctAction(action,kind,uid){toast('正在执行…','info');const d=await api(action,{method:'POST',body:{platform:kind,uid:uid}});showBatch(d);setTimeout(loadAccounts,1200);}
async function runAll(action,uid){if(!confirm('确定要对所有账号执行吗？'))return;toast('正在执行…','info');const d=await api(action,{method:'POST',body:{uid:''}});showBatch(d);setTimeout(loadAccounts,1300);}
function showBatch(d){if(d.error){toast(d.error,'err');return;}const rs=d.results||[];if(!rs.length){toast(d.message||'完成','ok');return;}rs.forEach(r=>toast((r.ok?'✓ ':'✗ ')+'['+(r.kind||'')+'] '+r.uid+'：'+r.msg,r.ok?'ok':'err'));}
async function delAcct(kind,uid){if(!confirm('确认删除该账号？'))return;const d=await api('delete',{method:'POST',body:{platform:kind,uid:uid}});toast(d.success?d.message:d.error,d.success?'ok':'err');setTimeout(loadAccounts,1500);}
function qrURL(u){return 'wb-api/qr?data='+encodeURIComponent(u);}
async function wbLogin(){show('正在获取 WorkBuddy 登录页…');const d=await api('wb-url');if(d.error){toast(d.error,'err');return;}
const u=d.url||'';
document.getElementById('loginShow').innerHTML='<div class="hint">请用<b>微信扫码</b>，或点下方「浏览器中打开」用手机号登录。若显示企业登录，请切到「个人登录」。</div>'+
'<div style="margin:12px 0;text-align:center"><div class="qr"><img src="'+qrURL(u)+'" alt="授权二维码" style="max-width:220px"/></div></div>'+
'<div style="text-align:center;margin-top:8px"><a href="'+esc(u)+'" target="_blank" rel="noopener">浏览器中打开</a></div>'+
'<p class="hint" id="wbWait">扫码或登录完成后请点击下方「完成登录」按钮，凭证将自动保存。</p>'+
'<div style="text-align:center;margin-top:10px"><button class="btn btn-ok" id="wbDoneBtn" onclick="pollWbDone()">完成登录</button></div>';
wbStop=0;document.getElementById('loginShow').style.display='';}
async function pollWbDone(){const btn=document.getElementById('wbDoneBtn');if(!btn)return;btn.disabled=true;btn.textContent='获取凭证中…';const d=await api('wb-poll',{method:'POST'});if(d.success){wbStop=1;show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error||'登录尚未完成，请确认已在微信扫码后重试');toast(d.error||'登录尚未完成','err');}btn.disabled=false;btn.textContent='重试完成登录';}

let wbTimer=null,wbStop=0;
async function autoPollWb(){if(wbStop)return;const d=await api('wb-poll',{method:'POST'});if(d.success){wbStop=1;clearInterval(wbTimer);show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);return;}
const em=(d.error||'').toLowerCase();if(em.includes('未完成')||em.includes('waiting')||em.includes('login ing'))return;wbStop=1;clearInterval(wbTimer);show(d.error||'登录失败');toast(d.error||'登录失败','err');}
let _traePollTimer=null;
function traeStopAutoPoll(){if(_traePollTimer){clearTimeout(_traePollTimer);_traePollTimer=null;}}
async function traeLogin(){
  show('正在启动 TraeWork 登录…');
  const d=await api('trae-url');
  if(d.error){toast(d.error,'err');return;}
  // 打开授权链接，并自动轮询回调是否已落盘；用户只需在 Trae 页登录，回来即完成。
  showBoxLink(d.url,'<b>TraeWork 登录</b>：点击下方链接在浏览器完成登录（支持手机号/账号/抖音扫码）。<br>'
    +'登录成功后授权页会<b>自动跳回本面板</b>，本弹窗会<b>自动检测并完成</b>，无需任何手动复制。<br>'
    +'（少数情况下若未自动跳回，可把地址栏中以 <code>'+esc(location.origin)+'/api/trae-cb?</code> 开头的链接粘贴到下方「回调链接」框；refreshToken 框为可选备用，一般不必填。）<br>'
    +'<a href="'+esc(d.url)+'" target="_blank">浏览器中打开授权链接</a>');
  addTraeManual();
  // 自动轮询：回调到达即自动完成
  const tryPoll=async()=>{
    if(!_traePollTimer) return; // 已被取消
    const r=await api('trae-poll',{method:'POST'});
    if(r&&r.success){traeStopAutoPoll();show(r.message);toast(r.message,'ok');setTimeout(loadAccounts,1500);return;}
    _traePollTimer=setTimeout(tryPoll,2000);
  };
  traeStopAutoPoll();_traePollTimer=setTimeout(tryPoll,2000);
}
async function addTraeManual(){const box=document.getElementById('loginShow');box.innerHTML+='<div style="margin-top:10px;text-align:left">'
+'<input id="traeCbUrl" placeholder="粘贴回调链接（'+esc(location.origin)+'/api/trae-cb?...）" style="width:100%;box-sizing:border-box;padding:8px;border-radius:6px;border:1px solid #2a2f3a;background:#0c0e12;color:#e6e9ef;font-size:12px"/>'
+'<button class="btn btn-ok" id="traeCbSubmit" style="margin-top:8px;width:100%">提交回调链接（自动失败时的备用）</button>'
+'<input id="traeRt" placeholder="可选：refreshToken（一般不必填，长期有效）" style="width:100%;box-sizing:border-box;padding:8px;margin-top:8px;border-radius:6px;border:1px solid #2a2f3a;background:#0c0e12;color:#e6e9ef;font-size:12px"/>'
+'<button class="btn btn-warn" id="traeRtSubmit" style="margin-top:8px;width:100%">用 refreshToken 登录（备用）</button>'
+'</div>';document.getElementById('traeCbSubmit').onclick=submitTraeManual;document.getElementById('traeRtSubmit').onclick=submitTraeRefresh;}
async function submitTraeManual(){const url=(document.getElementById('traeCbUrl')||{}).value||'';if(!url){toast('请先粘贴授权回调链接','err');return;}traeStopAutoPoll();show('正在用回调链接换 token…');const d=await api('trae-complete',{method:'POST',body:{url}});if(d.success){show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error||'换 token 失败');toast(d.error||'换 token 失败','err');}}
async function submitTraeRefresh(){const rt=(document.getElementById('traeRt')||{}).value||'';if(!rt){toast('请先粘贴 refreshToken','err');return;}traeStopAutoPoll();show('正在用 refreshToken 换 token…');const d=await api('trae-complete-refresh',{method:'POST',body:{refresh_token:rt}});if(d.success){show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error||'refreshToken 登录失败');toast(d.error||'refreshToken 登录失败','err');}}
async function pollTrae(){traeStopAutoPoll();show('正在确认 TraeWork 登录…');const d=await api('trae-poll',{method:'POST'});if(d.success){show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error||'TraeWork 登录尚未完成（可尝试上方手动粘贴回调链接）');toast(d.error||'TraeWork 登录尚未完成','err');}}
let qoderDoneOpts=null;
async function qoderLogin(){show('正在获取 Qoder 授权链接…');const d=await api('qoder-url');if(d.error){toast(d.error,'err');return;}
showBoxLink(d.url,'<b>Qoder 登录</b>：请在浏览器打开下方链接完成登录（支持账号/扫码/手机号）。登录完成后回到本页点击下方按钮。<br><a href="'+esc(d.url)+'" target="_blank">浏览器中打开授权链接</a>');addBtn('完成 Qoder 登录',pollQoder);}
async function pollQoder(){show('正在确认 Qoder 登录…');const d=await api('qoder-poll',{method:'POST'});if(d.success){show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error||'Qoder 登录尚未完成');toast(d.error||'Qoder 登录尚未完成','err');}}
async function poll(key){show('正在确认登录…');const d=await api(key,{method:'POST'});if(d.success){show(d.message);toast(d.message,'ok');setTimeout(loadAccounts,1500);}else{show(d.error);toast(d.error,'err');}}
function qrURL(u){return 'wb-api/qr?data='+encodeURIComponent(u);}
function show(m){document.getElementById('loginHint').textContent=m;}
function showBoxQR(url,note){document.getElementById('loginShow').innerHTML='<div class="qr"><img src="'+qrURL(url)+'" alt="扫码"/></div><div class="hint" style="margin-top:8px">'+note+'<br><a href="'+esc(url)+'" target="_blank">浏览器打开授权链接</a></div>';}
function showBoxLink(url,note){document.getElementById('loginShow').innerHTML='<div class="hint">'+note+'</div><p><a href="'+esc(url)+'" target="_blank">'+esc(url)+'</a></p>';}
function addBtn(label,fn){document.getElementById('loginShow').innerHTML+='<button class="btn btn-ok" id="loginDoneBtn" style="margin-top:8px">'+label+'</button>';document.getElementById('loginDoneBtn').onclick=fn;}
async function loadModels(){const info=document.getElementById('modelInfo');info.textContent='加载中…';const d=await api('models');const body=document.getElementById('modelBody'),empty=document.getElementById('modelEmpty');
if(d.error){info.textContent='模型接口不可用: '+esc(d.error);body.innerHTML='';empty.style.display='none';return;}
const data=d.data||[];body.innerHTML=data.map(m=>'<tr><td>'+(m.owned_by?('<span class="badge '+(m.owned_by==='traework'?'b-info':'b-ok')+'">'+esc(m.owned_by)+'</span> '):'')+'<code>'+esc(m.id)+'</code></td><td>'+(m.context_length||'—')+'</td><td>'+(m.max_output_tokens||'—')+'</td></tr>').join('');empty.style.display=data.length?'none':'block';info.textContent='共 '+data.length+' 个模型';}
async function loadSettings(){const d=await api('config');if(d.error){toast(d.error,'err');return;}const o=d.options||{};const set=(id,v)=>document.getElementById(id).value=(v===undefined||v===null)?'':v;
set('f_api_key',o.api_key);set('f_region',o.region);set('f_upstream_timeout',o.upstream_timeout);set('f_cooldown_hard_credit',o.cooldown_hard_credit);set('f_cooldown_soft_rate',o.cooldown_soft_rate);set('f_cooldown_err_threshold',o.cooldown_err_threshold);set('f_cooldown_err_cooldown',o.cooldown_err_cooldown);set('f_checkin_times',Array.isArray(o.checkin_times)?o.checkin_times.join(','):o.checkin_times);set('f_keepalive_hours',Array.isArray(o.keepalive_hours)?o.keepalive_hours.join(','):o.keepalive_hours);}
async function doLogout(){const d=await api('logout',{method:'POST'});if(d.clear_cookie){document.cookie=d.clear_cookie+'=;path=/;expires=Thu, 01 Jan 1970 00:00:00 GMT';}location.reload();}
function showLoginUser(){var u=(document.getElementById('loginUser'));if(u){var n=(document.getElementById('f_webui_user')||{}).value||'admin';u.textContent='已登录: '+n;u.style.display='inline-block';}var b=document.getElementById('btnLogout');if(b)b.style.display='inline-block';}
async function changeLogin(){const u=(document.getElementById('f_webui_user')||{}).value||'';const p=(document.getElementById('f_webui_pass')||{}).value||'';if(!u){toast('请填写登录名','err');return;}const d=await api('change-login',{method:'POST',body:{user:u,pass:p}});toast(d.message||d.error,d.success?'ok':'err');if(d.success){document.getElementById('f_webui_user').value='';document.getElementById('f_webui_pass').value='';}}
async function saveSettings(){const opt={};const get=id=>document.getElementById(id).value;
opt.api_key=get('f_api_key');opt.region=get('f_region');opt.upstream_timeout=get('f_upstream_timeout');opt.cooldown_hard_credit=get('f_cooldown_hard_credit');opt.cooldown_soft_rate=get('f_cooldown_soft_rate');opt.cooldown_err_threshold=get('f_cooldown_err_threshold');opt.cooldown_err_cooldown=get('f_cooldown_err_cooldown');opt.checkin_times=get('f_checkin_times');opt.keepalive_hours=get('f_keepalive_hours');
const d=await api('config',{method:'POST',body:{options:opt}});toast(d.message||d.error,d.success?'ok':'err');if(d.success)setTimeout(loadOverview,800);}
loadOverview(true);showLoginUser();</script></body></html>"""


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
def run_server():
    os.makedirs(AUTH_DIR, exist_ok=True)
    os.makedirs(STATE_DIR, exist_ok=True)
    opts = load_options()
    ok = G.start(opts)
    print("[AI-Proxy] serverd internal port %d started: %s" % (SRVD_PORT, "OK" if ok else "FAILED " + G.last_error))
    print("[AI-Proxy] Management UI on http://%s:%d" % (HOST, PORT))

    def _on_term(*_):
        G.kill()
        os._exit(0)
    signal.signal(signal.SIGTERM, _on_term)
    signal.signal(signal.SIGINT, _on_term)
    http.server.ThreadingHTTPServer((HOST, PORT), LoginHandler).serve_forever()


if __name__ == "__main__":
    run_server()