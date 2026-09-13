# ha-ai-proxy 技术文档

> 审计日期：2026-08-25
> 审计范围：`D:\ai-hub\integrations\ha-ai-proxy` 全量源码（Go 1.25 + Python 3 管理面板 + HA addon 打包）
> 审计方式：静态代码审计（当前环境无 Go 工具链，未执行 `go build`/`go vet`；测试文件已通读以确认覆盖）
> 上游内核：`github.com/rockswang/workbuddy-wild`（本项目 `src/` 即该 module 的源码）

---

## 1. 项目概述

`ha-ai-proxy` 是一个 **Home Assistant 插件（addon）**，把多个 AI 平台的个人账号（**WorkBuddy / CodeBuddy** 与 **TraeWork SOLO**）聚合成一个 **OpenAI 兼容 API**，对外提供统一入口。

核心能力：

| 能力 | 说明 |
|------|------|
| 多账号聚合 | 同一平台可挂多个账号，按"健康 + 积分最多"策略挑选 |
| 模型来源前缀路由 | 调用模型名带 `workbuddy/<model>` 或 `traework/<model>` 前缀，自动路由到对应上游 |
| 账号轮转 / 冷却状态机 | 余额不足长冷却、429 短冷却、连续错误冷却、登录态失效禁用 |
| 自动签到 | 每日指定时刻对所有账号签到 + 刷新积分 + 解冻 |
| Token 保活 | 每日整点刷新 access token；session 失效自动禁用 |
| 扫码 / 回调登录 | WorkBuddy 设备流扫码、TraeWork PKCE 浏览器回调登录 |
| 管理 Web UI | 内嵌暗色面板，查看账号/积分/状态、登录、签到、刷新、删除、改设置 |

设计定位：**个人自用聚合代理**，依赖平台个人账号的 OAuth 凭证，非官方 API Key 体系。

---

## 2. 整体架构

进程分两层，容器内由 `run.sh` 拉起 Python 管理面板，面板再托管 Go 服务：

```
┌──────────────────────────────────────────────────────────────────┐
│  HA 容器 (ha-ai-proxy)                                              │
│                                                                     │
│   run.sh  →  python3 /app/login_ui.py  (PID 1, 监听 0.0.0.0:7863)  │
│                                                                     │
│   ┌── login_ui.py (ThreadingHTTPServer :7863) ──────────────────┐   │
│   │  • 管理 Web UI (HTML 内嵌)                                  │   │
│   │  • /api/* 管理接口（overview/accounts/config/login/cb...）   │   │
│   │  • /v1/* 反向代理 → serverd(:7864)  （SSE 流式透传）          │   │
│   │  • 托管 serverd 子进程生命周期（热重启）                      │   │
│   └───────────────┬──────────────────────────────────────────────┘   │
│                   │ 子进程 (Popen)                                     │
│   ┌───────────────▼──────────────────────────────────────────────┐   │
│   │  serverd (Go, 监听 0.0.0.0:7864)  OpenAI 兼容 HTTP            │   │
│   │   runtimes: { workbuddy, traework }                          │   │
│   │   每个 runtime = Pool + Upstream + Scheduler                  │   │
│   │     • pool.Pool   账号池 + 冷却状态机 + state.json 持久化     │   │
│   │     • upstream.*  WorkBuddy 上游封装                         │   │
│   │     • traework.* TraeWork SOLO 上游封装                      │   │
│   │     • scheduler.* 每日签到 + token keepalive                 │   │
│   └───────────────┬──────────────────────────────────────────────┘   │
│                   │ 定时 / 按需调用                                   │
│   login (Go)  logintrae (Go)  ctl (Go)   —— 均被面板当子进程调用     │
└───────────────────┼──────────────────────────────────────────────────┘
                    │ HTTPS（逆向自官方客户端）
   ┌────────────────▼──────────┐      ┌──────────────────────────────┐
   │ WorkBuddy / CodeBuddy 上游  │      │ TraeWork SOLO 上游            │
   │ copilot.tencent.com (CN)   │      │ Agent: trae-api-cn.mchost.gury│
   │ www.codebuddy.cn (billing) │      │ UG: api.trae.cn              │
   │ www.workbuddy.ai (global)  │      │ OAuth: api.trae.com.cn       │
   └────────────────────────────┘      └──────────────────────────────┘
```

**端口分配**
- `7863/tcp`：对外公开 + ingress 端口，承载 OpenAI API 与管理 UI（Python 层）
- `7864/tcp`：内部端口，仅容器内能访问，承载 Go `serverd`（由 `login_ui.py` 生成的 `config.json` 固定为 `0.0.0.0:7864`）

**进程依赖**
- 面板是父进程，负责 `serverd` 的启动、热重启（`G.restart`）、终止（SIGTERM/SIGINT → `G.kill()`）
- 登录工具（`login`/`logintrae`/`ctl`）是一次性子进程，由面板通过 `subprocess.run` 调用，JSON 走 stdout，日志走 stderr

---

## 3. 目录结构与模块职责

```
ha-ai-proxy/
├── config.yaml        HA addon 清单（名称/端口/ingress/options/schema）
├── schema.yaml        options 字段类型定义（与 config.yaml schema 重复）
├── build.yaml         HA addon 构建描述（aarch64/amd64，golang:1.25-alpine）
├── Dockerfile         多阶段构建：golang:1.25-alpine 编译 4 个二进制 + alpine 运行时
├── run.sh             入口：建持久化目录 → exec python3 login_ui.py
├── login_ui.py        管理面板 + OpenAI API 统一入口（Python 3）
└── src/
    ├── go.mod / go.sum   module github.com/rockswang/workbuddy-wild, go 1.25.0
    └── cmd/
        ├── serverd/main.go   无头服务入口，装配双平台 runtime，暴露 OpenAI HTTP
        ├── ctl/main.go       管理 CLI（accounts/credits/checkin/refresh），子进程
        ├── login/main.go     WorkBuddy CN OAuth 登录（url/poll），子进程
        └── logintrae/main.go TraeWork 登录（url/complete），子进程
    └── internal/
        ├── auth/       凭证解析（嵌套/扁平双形态）、region 判定、原子写回、账号池加载
        ├── config/     JSON 配置 + 环境变量覆盖 + 原子写回 + 时间解析
        ├── provider/  平台 Kind + 上游最小接口 + 错误分类枚举（ErrKind）
        ├── svc/       双平台运行时装配（Pool+Upstream+Scheduler 各两份）
        ├── pool/      账号池：挑选策略 + 冷却/禁用状态机 + state.json 持久化
        ├── scheduler/ 定时任务：每日签到 + token keepalive，支持运行中改签到时刻
        ├── server/     OpenAI 兼容 HTTP handler，按模型前缀路由 + 账号轮转
        ├── login/     WorkBuddy 登录库（被 cmd/login 与未来 GUI 复用）
        ├── login_trae/ TraeWork 回调登录库（PKCE + 设备指纹 + 多回调形态解析）
        ├── upstream/   WorkBuddy/CodeBuddy 上游 HTTP 封装 + 错误分类 + SSE 透传/聚合
        └── traework/   TraeWork SOLO 上游封装（自定义 SSE 翻译 + 设备指纹头 + 签到）
```

**关键依赖（go.mod）**：`wailsapp/wails/v2`（声明但未在 addon 编译路径使用，见 §9 风险）、`energye/systray`、`golang.org/x/sys`。其余为间接依赖（echo、webview2 等，来自 wails）。

---

## 4. 核心数据流

### 4.1 一次 chat 请求

1. 客户端 → `7863/v1/chat/completions`（带 `Authorization: Bearer <api_key>`）
2. `login_ui.py._proxy` 透传到 `serverd:7864/v1/chat/completions`
3. `server.Handler.chatCompletions`：
   - `withAuth` 校验 api_key（空则不校验）
   - 读 body，从 `model` 字段解析前缀（`workbuddy/<m>` / `traework/<m>`），否则 400
   - `rewriteModel`：把 body 里的 `model` 改写为去掉前缀的内层模型名
   - 最多 `MaxRotate`(默认 3) 次尝试：
     - `Pool.PickExcluding(tried)` 挑健康且积分最高的账号
     - 若 `NeedsRefresh(RefreshSkew=10min)` → 先 `RefreshToken` + `SaveAtomic`
     - `Upstream.ChatStream` 发请求（WorkBuddy 强制 stream，TraeWork 强制 stream）
     - 非 2xx → `Upstream.Classify` 判定错误类别 → 冷却/禁用 → 轮换下一账号
     - 2xx → 流式走 `Upstream.Stream`（SSE 透传）或非流式走 `Upstream.Aggregate`（聚合为单条 OpenAI 响应）
4. 全部账号不可用 → 503 `no_healthy_account`

### 4.2 模型路由与命名

- **必须带前缀**：`workbuddy/glm-5.2`、`traework/glm-5.2`。无前缀或非 `workbuddy|traework` 前缀 → 400 `invalid_model`
- 内层模型名经 `rewriteModel`（WorkBuddy）或 `traework.PrepareBody` 改写为上游实际模型标识
- `GET /v1/models`：对每个"已接入账号"的平台列出模型（`动态获取` 失败则用 `静态兜底列表`）

### 4.3 账号冷却状态机（pool）

| 状态 | 触发 | 时长（默认） | 行为 |
|------|------|------|------|
| `CoolHard` | 余额/权益不足（402 或 body 含关键词） | `hard_credit`=12h | 长冷却，签到解冻 |
| `CoolSoft` | 429 / 上游 404 | `soft_rate`=60s | 短冷却，不累计 errCount |
| `CoolErr` | 连续非余额/非 429 错误达阈值 | `err_cooldown`=10m | 累计 errCount ≥ `err_threshold`(3) |
| `Disabled` | session 失效（401+12153 等） | 永久 | 需重新登录或替换文件 |

- `Pick` 跳过 `disabled` 与 `until` 未过期者；同健康下选 `credits` 最大者
- 状态持久化到 `state-workbuddy.json` / `state-traework.json`（tmp+rename 原子写）
- 签到成功后 `ReenableIfCredits`：余额 > 0 且非禁用 → 解除冷却

---

## 5. 双平台上游协议

### 5.1 WorkBuddy / CodeBuddy（upstream 包）

| 接口 | 路径 | Base |
|------|------|------|
| Chat | `/v2/chat/completions` | `copilot.tencent.com`(CN) / `www.workbuddy.ai`(global) |
| 模型 | `/console/enterprises/personal/models` | 同 Chat |
| 刷新 | `/v2/plugin/auth/token/refresh` | 同 Chat |
| 积分 | `/v2/billing/meter/get-user-resource` | `www.codebuddy.cn`(CN) / `workbuddy.ai` |
| 签到 | `/v2/billing/meter/daily-checkin` | 同 Billing |

- 统一信封 `{code,msg,data}`，`code!=0` 视为业务错误
- SSE 已是 OpenAI 兼容格式 → **直接透传**（`upstream.Stream`）
- 请求改写（`upstream.PrepareBody`）：
  - 强制 `stream:true`
  - `developer` 角色 → `system`（上游对 developer 触发内容过滤误杀）
  - `tool_choice` 归一化（上游是 string 字段，对象形式会 400 code=11101）
- 错误分类关键词：余额不足（中英文双通道）、12153/session 失效、429、404、5xx

### 5.2 TraeWork SOLO（traework 包）

| 接口 | 路径 | Base |
|------|------|------|
| Chat | `/api/agent/v3/llm_utils_chat` | `trae-api-cn.mchost.gury` |
| 模型 | `/api/ide/v1/get_detail_param` | 同 Agent |
| 刷新 | `/cloudide/api/v3/trae/oauth/ExchangeToken` | `api.trae.com.cn` |
| 用户信息 | `/cloudide/api/v3/trae/GetUserInfo` | 同 OAuth |
| 签到状态/领取 | `/trae/api/v2/ug/checkin_credits/status\|claim` | `api.trae.cn` |
| 积分 | `/trae/api/v2/pay/web_user_ent_usage` | `api.trae.cn` |

- **自定义 SSE 协议**（event:metadata/output/extra_info/token_usage/done/error），需翻译为 OpenAI SSE（`traework.Stream` / `Aggregate`）
- 鉴权头：`Cloud-IDE-JWT` + `X-Cloudide-Token` + `X-Ide-Token`，外加 **设备指纹头**（`x-device-brand`/`x-os-version`/`x-device-id` 等，UG 签到/积分接口校验，缺任一环节会以 9074 拒绝）
- 请求改写（`traework.PrepareBody`）：`config_name`/`model` 设为内层模型、`function=solo_work_lite`、assistant 的 `function`→`function_call`、tools 的 parameters 序列化为字符串
- 登录：PKCE(S256) + ECDSA P256 设备公钥（`authcode.go`），回调多种形态（refreshToken / userJwt / authCodeInfo）容错解析
- **关键风险**：Agent host `trae-api-cn.mchost.gury` 为**非官方第三方域名**，`ClientID/ClientSecret` 为逆向所得固定值（`ClientSecret:"-"`），上游一旦变更/封锁即整体失效（见 §9）

---

## 6. 登录流程

### 6.1 WorkBuddy（设备流，无 PKCE）
1. `login url` → POST `/v2/plugin/auth/state?platform=CLI` 拿 `state` + `authUrl`，state 落 `/tmp/wb2api-login-state.json`
2. 面板 iframe 嵌入 `authUrl`，用户浏览器登录
3. 面板轮询 `login poll` → GET `/v2/plugin/auth/token?state=` 拿 token 包；再 GET `/v2/plugin/login/account` 拿 uid/nickname
4. 写 `workbuddy-<uid>.json`（嵌套形，chmod 600），热重启 serverd

### 6.2 TraeWork（PKCE + 浏览器回调）
1. `logintrae url -state -callback` → 生成授权 URL（`www.trae.cn/authorization` + PKCE challenge + 设备指纹），state 落 `/tmp/ai-proxy-trae-login-state.json`
2. 用户浏览器打开 URL 登录，登录成功**重定向/回调到面板 `/api/trae-cb`**
3. 面板 `_handle_trae_cb` 解析回调（refreshToken / userJwt / authCodeInfo 多形态），`logintrae complete` 换 token + 取账号信息
4. 写 `trae-<uid>.json`，热重启 serverd

### 6.3 凭证磁盘形态
- 兼容**嵌套形** `{"auth":{...},"account":{...}}` 与**扁平形** `{"accessToken":...}`（`auth.Parse`）
- 写回统一为嵌套形（tmp+rename，0600），保证 CPA 插件可读
- `RefreshToken` 头 `X-Refresh-Token` **仅在 refresh 端点出现**（代码注释明确为安全红线）

---

## 7. 配置项

### 7.1 HA addon options（config.yaml）

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `api_key` | password | `""` | OpenAI 客户端调用所需的 Key；**空=不鉴权** |
| `region` | cn\|global | `cn` | WorkBuddy 区域（影响账号过滤与上游域） |
| `cooldown_hard_credit` | str | `12h` | 余额不足冷却 |
| `cooldown_soft_rate` | str | `60s` | 429 冷却 |
| `cooldown_err_threshold` | int | `3` | 连续错误触发阈值 |
| `cooldown_err_cooldown` | str | `10m` | 连续错误冷却 |
| `checkin_times` | str | `09:00,21:00` | 每日签到时刻（HH:MM，逗号分隔） |
| `keepalive_hours` | str | `22` | token 保活整点小时 |
| `upstream_timeout` | int | `120` | 上游 HTTP 超时（秒） |

### 7.2 serverd 运行时配置（Python `build_config` 生成 `/data/config.json`）
- `listen.{host,port}` = `0.0.0.0:7864`
- `api_key` / `auth_dir`=`/data/auths` / `state_file`=`/data/data/state.json`
- `cooldown.{hard_credit,soft_rate,err_threshold,err_cooldown}`
- `schedule.{checkin_times[],keepalive_hours[]}`
- `upstream.timeout_seconds`
- 支持 `WB2A_*` 环境变量覆盖（LISTEN/API_KEY/AUTH_DIR/STATE_FILE/REGION/HARD_CREDIT/SOFT_RATE/ERR_THRESHOLD/ERR_COOLDOWN/TIMEOUT_SECONDS）

> 注意：`config.Default()` 里 `api_key` 默认是 `"WorkBuddy2API"`；但 addon 路径由 options.json 驱动，`api_key` 默认 `""` → 实际**默认不鉴权**（靠 HA ingress 鉴权兜底，见 §8/§9）。

---

## 8. 管理 Web UI 接口（login_ui.py）

| 方法 | 路径（匹配 `/api/` 或 `/wb-api/`） | 作用 |
|------|------|------|
| GET | `/healthz` | 健康检查（返回 `OK`，HA HEALTHCHECK 用） |
| GET | `overview` | 服务状态/账号统计 |
| GET | `accounts` | 账号列表（经 `ctl accounts`） |
| GET | `models` | 代理到 serverd `/v1/models` |
| GET/POST | `config` | 读取/保存设置（写 options.json + 热重启） |
| GET | `wb-url` / POST `wb-poll` | WorkBuddy 登录 |
| GET | `trae-url` / POST `trae-cb` | TraeWork 登录回调 |
| POST | `credits` / `checkin` / `refresh` | 账号批量动作（经 `ctl`） |
| POST | `delete` | 删除账号（删文件 + 热重启） |
| 任意 | `/v1/*` 或 `/status` | 反向代理到 serverd（SSE 流式逐块透传） |

Web UI 为单文件内嵌 HTML（暗色主题），含概览/账号/模型/设置四个 Tab，含登录 iframe、签到/刷新/删除按钮、设置表单。

---

## 9. 安全与风险审查（审计重点）

### 9.1 安全性（正面）
- Token 文件 `chmod 600`（`_write_auth_file` / `SaveAtomic`），state 文件同样 0600
- `RefreshToken` 仅在 refresh 端点作为 `X-Refresh-Token` 发送；chat 路径绝不携带
- 访问令牌读写全程持 `auth.mu` 读写锁，防止并发写回半更新 token
- 所有磁盘写均 tmp+rename 原子替换，避免损坏
- 上游 TLS 直连，未禁用证书校验

### 9.2 风险与缺陷（按严重度）

#### 🔴 高
1. **容器时区为 UTC，签到/保活时刻偏离本地时间**
   - Dockerfile 仅 `apk add tzdata`，**未设 `ENV TZ=Asia/Shanghai`**；`run.sh`/`login_ui.py` 也未设。
   - 后果：配置 `09:00` 实际在 **09:00 UTC = 17:00 北京时间** 触发签到/保活。对中国用户是隐性功能错乱。
   - 建议：Dockerfile 加 `ENV TZ=Asia/Shanghai`，或在 `scheduler` 按配置时区解析。

2. **TraeWork 强依赖非官方第三方 host `trae-api-cn.mchost.gury`**
   - 该域名非 Trae 官方，随时可能失效/被封/被墙；`ClientID/ClientSecret`（`"-"`）为逆向固定值。
   - 后果：TraeWork 通道整体可用性脆弱，且可能在用户无感知时静默失效。
   - 建议：抽成可配置项（options 加 `trae_agent_host`），支持用户自建/替换；加可用性监控与告警。

3. **TraeWork 任意 401 → 永久禁用账号**
   - `traework.Classify` 中 `status==401` 一律返回 `ErrSessionDead`，`handler` 据此 `Pool.Disable`（永久）。
   - 后果：偶发/瞬态 401（如 token 刚好过期但 refresh 未先跑）会把账号打为禁用，需人工重登。WorkBuddy 侧 refresh 在请求前已先跑，TraeWork 侧 chat 路径**未先 refresh**（`traework.ChatStream` 直接发，无前置 `NeedsRefresh` 检查）。
   - 建议：TraeWork chat 路径在发请求前也加 `NeedsRefresh` 预判刷新；401 首次降级为冷却而非直接禁用。

#### 🟡 中
4. **WorkBuddy 流式响应受 `upstream_timeout` 截断**
   - `upstream.Client.ChatStream` 用 `c.HTTP`（Timeout=120s），而 `http.Client.Timeout` 包含**读取响应体**时间。
   - 后果：单次 chat 流式输出 > 120s 会被中途切断（客户端收到截断流）。TraeWork 的 `StreamHTTP` 未设 Timeout（无此问题），两平台行为不一致。
   - 建议：流式请求使用**不设 Timeout** 的专用 client（仅对建连/首字节设 `ResponseHeaderTimeout`），与 TraeWork 对齐。

5. **默认无 API 鉴权**
   - 默认 `api_key=""` → `serverd` 不校验。仅由 HA ingress 提供外层鉴权。
   - 后果：若 addon 以 `host_network` 或端口直接暴露到 LAN，OpenAI API 可被任意调用（消耗账号额度）。
   - 建议：默认生成一个随机 api_key；或在文档中明确"禁止非 ingress 直连"。

6. **`ctl` 与 `serverd` 并发刷新同一账号文件**
   - 两者是独立进程，均可 `SaveAtomic` 同一 `auth` 文件；tmp+rename 保证不损坏，但**后写覆盖前写**，可能丢失 refresh 轮转后的新 refreshToken。
   - 建议：刷新走单一权威（建议仅 serverd 在内存态刷新，ctl 只读），或加文件锁。

#### 🟢 低 / 维护性
7. **代码重复**：`cmd/login/main.go` 与 `internal/login/login.go` 重复实现 WorkBuddy OAuth；`sortInts` / `truncate` 在多个包多份拷贝；`server.Handler` 中 `dynamicModelsCache` 全局变量与 `rt.models` **双缓存并存**（注释标注"保留给旧测试"），易混淆。
8. **遗留字段**：`scheduler.Config.CheckinHours`（旧整点）与 `CheckinMinutes`（新分钟精度）并存；`config.Schedule.CheckinHours/CheckinTimes` 双形态兼容逻辑较绕，但功能正确。
9. **wails/systray 依赖声明但未用**：go.mod 引入 `wailsapp/wails/v2`、`energye/systray`，addon 编译路径（serverd/ctl/login/logintrae）均未使用，徒增构建体积与 `go.sum` 体积。疑似从桌面版（GUI）fork 而来，可精简。
10. **`region=global` 仅影响 WorkBuddy**；TraeWork 上游 host 全部硬编码 CN，无 global 适配。
11. **`KeepaliveHours`/`checkin_times` 在 Python 与 Go 两侧各自 normalize**，schema.yaml 与 config.yaml 的 `schema` 段重复。
12. **无结构化日志/可观测性**：全靠 `log.Printf` 到 stderr；无指标、无请求 ID、无访问日志（面板 `log_message` 被禁用）。

---

## 10. 测试覆盖

| 测试文件 | 行数 | 覆盖内容 |
|----------|------|----------|
| `auth/auth_test.go` | 109 | 嵌套/扁平凭证解析、region 判定、SaveAtomic |
| `config/config_test.go` | 158 | Listen 解析、时间解析、默认值、env 覆盖、normalize |
| `pool/pool_test.go` | 227 | 挑选策略、冷却/禁用/解冻、错误计数、状态持久化 |
| `scheduler/scheduler_test.go` | 306 | 签到/保活触发时刻、运行中改时刻、NextFire |
| `server/handler_test.go` | 430 | 模型路由、鉴权、轮转、错误分类、SSE 聚合 |
| `upstream/client_test.go` | 233 | 错误分类、积分聚合、刷新、body 改写 |
| `upstream/sse_test.go` | 171 | OpenAI SSE 聚合（content/reasoning/tool_calls） |
| `traework/client_test.go` | 121 | 签到/积分/模型解析/错误分类 |

覆盖较均衡，核心逻辑（路由、冷却、SSE、签到）均有单测。**缺口**：登录流程（PKCE 交换、回调解析）无单测；跨进程集成（面板↔serverd 代理）无测试；上游真实网络行为靠 mock。

> ⚠️ 本次审计环境**无 Go 工具链**，未能执行 `go test`/`go vet` 验证编译与通过情况，仅静态审阅测试代码。建议在 CI 中加入 `go vet ./... && go test ./...`。

---

## 11. 部署形态

- **形态**：HA addon（Supervisor 托管）。`config.yaml` 含 `hassio_api`/`ingress`/`panel_icon` 等字段。
- **构建**：`build.yaml` 双架构（aarch64→arm64 / amd64）；Dockerfile 多阶段：golang:1.25-alpine 编译 4 个二进制 → alpine:3.20 运行时（bash/curl/jq/python3/ca-certificates/tzdata）。
- **启动**：`run.sh` → `python3 login_ui.py`（PID 1）→ 拉起 `serverd`。
- **持久化**：`/data/auths`（凭证，rw）、`/data/data`（state.json、options.json、config.json）。
- **健康检查**：`wget http://127.0.0.1:7863/healthz`。
- **端口映射**：`7863/tcp`（ingress + 容器端口一致）。

> 注：SOUL.md HA 铁律要求"本地 custom_components 改完必须 SCP 到远程"。本插件是**独立容器**，不走 custom_components 路径，部署即重建镜像/重新 build，无 SCP 环节。

---

## 12. 改进建议（可落地）

1. **时区**（必改）：Dockerfile 加 `ENV TZ=Asia/Shanghai`，让签到/保活按北京时间触发。
2. **TraeWork 可用性与 401 处理**：chat 前先 `NeedsRefresh` 预判刷新；401 首次降级冷却；Agent host 抽成可配置项 + 加通道健康探测。
3. **流式超时**：WorkBuddy 流式请求改用无 Timeout 专用 client（仅 `ResponseHeaderTimeout`），与 TraeWork 对齐。
4. **默认安全**：未设 api_key 时生成随机值并写入 options，避免无鉴权暴露。
5. **精简依赖**：移除 wails/systray（addon 不用），减小镜像与 go.sum。
6. **去重**：合并 `cmd/login` 与 `internal/login`；收敛 `sortInts`/`truncate`；清理 `dynamicModelsCache` 双缓存。
7. **可观测性**：面板增加访问日志/请求 ID；serverd 增加结构化日志与简单指标端点（如 `/metrics` 或 `/status` 扩字段）。
8. **并发刷新**：统一刷新权威（建议 serverd 内存态刷新，ctl 只读），或加 `flock`。
9. **CI**：加 `go vet` + `go test` 门禁；补登录流程单测（PKCE/回调解析）。

---

## 附录 A：关键常量速查

- WorkBuddy Chat Base：`https://copilot.tencent.com`（CN）/ `https://www.workbuddy.ai`（global）
- WorkBuddy Billing Base：`https://www.codebuddy.cn`（CN）
- TraeWork Agent：`https://trae-api-cn.mchost.gury`；UG：`https://api.trae.cn`；OAuth：`https://api.trae.com.cn`
- TraeWork ClientID：`en1oxy7wnw8j9n`；Function：`solo_work_lite`；DefaultConfig：`glm-5.2`
- 默认冷却：hard=12h / soft=60s / err 阈值=3 / err 冷却=10m；轮转上限 MaxRotate=3；刷新预判窗口 RefreshSkew=10min

## 附录 B：审计结论

代码整体**结构清晰、分层合理、核心逻辑有单测覆盖**，作为个人聚合代理功能完整。主要短板集中在**运维健壮性**而非功能正确性：① 时区导致定时任务时刻偏移（功能性 bug）；② TraeWork 通道对非官方 host 的强依赖与过激的 401 禁用策略（可用性风险）；③ 默认无鉴权 + WorkBuddy 流式超时截断（安全/体验风险）；④ 少量历史冗余代码与未被使用的 GUI 依赖。上述 1~5 项为优先修复项。
