# TraeWork 登录排查与修复记录（已更正）

> 更新：2026-08-27
> 范围：AI Proxy addon 中 TraeWork（SOLO）渠道登录
> **更正声明**：旧版本文档称「授权页硬性绑定 127.0.0.1 白名单、公网回调架构上不可能」。
> 该结论**错误**——真实抓包证明授权页可向任意可达回调地址交付 token（含公网域名与 localhost 扫码）。
> 本版基于真实 HAR + 上游源码（wild-work@ad32896）重写。

---

## 1. 真实登录机制（HAR 证据）

抓包得到两条真实登录链路，均**非** 127.0.0.1 独享：

### 1.1 账号密码登录（mon.zijieapi.com）
- 授权页把凭证 POST 到公网回调域名 `mon.zijieapi.com`（不是 127.0.0.1）。
- 回调体含 `refreshToken` / `userJwt` / `authCodeInfo`。

### 1.2 抖音扫码登录（localhost 抓包）
- 扫码成功后回调落在 `localhost`，但同样走 `refreshToken`+设备公钥换 token。
- 证明「可达即交付」，与具体 host 无关。

### 1.3 协议端点（internal/traework）
- PKCE 换 token：`POST {api.trae.cn}/trae/api/v3/oauth/ExchangeToken`
  body 带 `AuthCode` + `CodeVerifier` + ECDSA P256 设备公钥 `DevicePublicKey`。
- refreshToken 换 token：`host + /cloudide/api/v3/trae/oauth/ExchangeToken`（上游 RefreshToken 用 `EpExchange`）。
- 设备公钥：`ExchangeAuthCode` 已正确发送 `DevicePublicKey`+`DeviceInfo`（与 HAR 一致）。

---

## 2. addon 与桌面版的真实差异

上游 `wild-work` 是**桌面 daemon**：`login_trae.Start()` 在本机开 `127.0.0.1:0` 监听，
授权页回调地址恰好匹配桌面 JS 里的 `127.0.0.1` 正则（因为同机）。

addon 是**异地容器**：没有本机浏览器/监听。差异只有「回调地址落在哪」，
**不是**「协议被白名单拒绝」。正确解法 = 让授权页回调到一个**容器公网可达**的地址。

| 维度 | 桌面版 | addon |
|------|--------|-------|
| 回调地址 | `http://127.0.0.1:<port>/authorize`（同机） | `http://<ha>:7863/api/trae-cb/authorize`（公网，经 ingress） |
| 监听 | 本机 listener | 面板 `login_ui.py` 的 `/api/trae-cb` 路由 |
| 换 token | `Poll` 读 state → `ExchangeAuthCode`/`RefreshToken` | 同左，或 `CompleteRefresh` 直换 |

---

## 3. 当前落地方案（addon 独有，手维护）

源码分层（见 SYNC.md）：
- `internal/login_trae/login.go` —— **上游纯副本**（sync 生成，带 GENERATED 头）。
- `internal/login_trae/addon_extras.go` —— **addon 独有**，手维护，不被 sync 覆盖：
  - `AuthURL(statePath, callbackBase)`：生成授权 URL，回调指向公网 `callbackBase/authorize`。
  - `StartOn(client, statePath, listenAddr, callbackBase)`：容器内可监听 0.0.0.0，但授权 URL 仍用公网回调。
  - `Complete(client, statePath, rawURL)`：解析浏览器回调 URL → 写 state → 委托上游 `Poll` 换 token。
  - `CompleteRefresh(client, refreshToken, host)`：直接 refreshToken 换 token（**最稳兜底**，不依赖浏览器回调）。
- `login_ui.py`：面板提供「公网授权链接」+「用 refreshToken 登录」两入口；修正了旧版「复制 127.0.0.1 链接」的误导文案。

### 3.1 路由
- `GET/POST /api/trae-cb` —— 公网回调（浏览器登录后跳回，带 refreshToken/authCodeInfo）。
- `GET/POST /api/trae-complete` —— 手动粘贴回调 URL 兜底。
- `GET/POST /api/trae-complete-refresh` —— refreshToken 直登。

### 3.2 验证清单
- 新镜像含 `logintrae`/`loginqoder`；`/api/overview` 出现 `qd_*` 指标。
- `/api/trae-complete`（无 url）返回 400（路由存在，非 404）。
- 优先用 refreshToken 直登（抓包 refreshToken 有效期至 2027-02，最稳）；
  或公网授权链接 → 自动回调 `/api/trae-cb` → 面板点「完成 TraeWork 登录」。

---

## 4. 同步策略（重要）

addon 的 Go 核心 **vendor 复制**自上游（上游 internal/ 受 Go 语言级封锁，外部 module 不可 import）。
同步方式见 `SYNC.md`：`sync_vendor.sh` 从上游固定 commit 生成同源包（带 GENERATED 头），
`login_trae/addon_extras.go` 与 `internal/svc` 受保护、不被覆盖。
**Trae 协议层自 13e7b64 后上游未再改动**，近期上游更新（粘性路由/CI/--no-tray）不影响 Trae 登录。

---

## 5. 关键参考
- 上游：`github.com/rockswang/wild-work`（对齐 commit ad32896）
- PKCE 端点：`POST {api.trae.cn}/trae/api/v3/oauth/ExchangeToken`
- refresh 端点：`{host}/cloudide/api/v3/trae/oauth/ExchangeToken`
- 同步基线：`SYNC.md`
