# Changelog

## v1.1.0b14 (2026-09-15) 测试版

本轮包含三类改动：独立审计发现的缺陷修复、CI/CD 基建、以及若干安全加固。

### 修复
- **签到/保活时刻偏移 8 小时**：容器缺 `ENV TZ`，`time.Now()` 取到 UTC，
  options 里配置的 `09:00` 实际在 **17:00**（北京时间）触发。Dockerfile 补
  `ENV TZ=Asia/Shanghai`（`tzdata` 原本已装）。**升级后触发时刻会变回配置值，
  属预期行为变化。**
- **TraeWork 账号被误永久禁用**：token 预刷新窗口仅 10 分钟，请求途中 token
  失效时上游返回 401 → 判定 session dead → 账号被永久禁用、需人工重登。
  现按平台区分窗口（TraeWork 24h / WorkBuddy 保持 10min）。
- **长回答被中途截断**：流式请求使用了带 `Timeout` 的 HTTP client，而
  `http.Client.Timeout` 包含**读取响应体**的时间，单次输出超 120s 即被切断。
  现流式改用无总时长上限的 client，首字节由 `ResponseHeaderTimeout` 约束。
- **面板列出不可用的模型**：未过滤 `is_custom_model`（第三方代理模型需额外
  授权），用户选中后必然失败。现过滤该类型（对齐上游 `abb9b0b`）。
- **凭证文件写入竞态（可能丢失 refreshToken）**：凭证用**固定**临时文件名
  （`path.tmp`），而 `serverd` 与 `ctl` 是**两个独立进程**、都会写同一份文件。
  并发下会出现 rename 竞争（实测并发 800 次失败 292 次）；更严重的是上游
  refresh **会轮换 refreshToken**，后落盘者覆盖先落盘者，若落盘的是已失效的
  那个，账号下次刷新即失败。现改用新增的 `internal/atomicfile`：唯一临时
  文件名 + 跨进程 `flock` + fsync。state 文件同样处理。
- **流式响应中途的上游错误未被识别**：上游并非只在 `status>=400` 时报错
  （同包三处代码都显式处理 `code != 0` 的 200 响应）。流式路径此前只做逐行
  透传，会把 `{"code":1,"msg":"余额不足"}` 这类非 SSE 内容原样写给客户端，
  且**不触发账号冷却**，后续请求继续选中它。现透传前嗅探首行，非 SSE 帧按
  上游错误信封解析并走既有错误分类。
- **流式错误被完全丢弃**：`_ = Upstream.Stream(...)` 忽略返回值，且
  `NoteSuccess` 在流式开始**之前**调用 —— 即使流失败，账号也被标记为健康。
  现改为先透传、成功后才记成功，失败时记录日志并按类别冷却/禁用账号。

### 安全
- **管理接口默认对局域网开放**：`config.yaml` 默认 `webui_user/pass` 为空，
  面板登录未启用时，鉴权回退为「来源在内网即放行」，而 add-on 的 `7870/tcp`
  映射到宿主机 —— 同网段任意设备可**无认证**调用管理接口（账号列表、刷新
  令牌、修改配置）。根因是 ingress 转发（`172.30.x`）与 LAN 直连
  （`192.168.x`）**源 IP 同属内网，无法仅凭 IP 区分**。现未启用登录时
  **只信任 ingress 转发与本机回环**。
  **注意：若此前通过非 ingress 方式（如直接访问 `http://<HA>:7870`）使用
  管理界面，升级后会被拒绝 —— 请在设置页配置面板登录账号/密码，或改走
  ingress 入口。**
- **面板会话 token 可预测**：原用 `sha256(time.time())` 生成，熵几乎全部
  来自时间戳，可被枚举。现改用 `secrets.token_urlsafe(32)`（约 258 bit 熵），
  落盘权限收紧为 `0600`，比较改用 `hmac.compare_digest`（原先的 `==` 会短路，
  可据响应时间逐字节推断 token）。

### 构建与发布
- **预构建镜像**：改用 HA 官方 builder actions 在 CI 构建 amd64/aarch64
  镜像并推送 `ghcr.io/c3h3-ai/ai-proxy`，`config.yaml` 增加 `image` 字段。
  Supervisor 直接拉取镜像，**不再在用户设备上编译**（此前需拉 Go 镜像并编译
  5 个二进制，在树莓派上可能十余分钟且易受网络影响失败）。
- **CI/CD 门禁**：新增 `go-ci`（build/vet/test）、`pr-validate`（分支/标题/
  描述/单一变更类型）、`pr-label`（自动打标）、`release`（tag + 草稿 release）、
  `stale`、`sync-labels`。此前仓库无任何 CI。
- **移除 `build.yaml`**：legacy builder 配置，自 Supervisor 2026.04.0 起
  **不再被读取**，其 `build_from` / `args.BUILD_VERSION` 均为死配置。

### 内部
- **上游同步保护**：`sync_vendor.sh` 此前会静默覆盖 addon 对上游文件的改动，
  实测跑一次即导致**编译失败**（`svc.go` 引用了被覆盖掉的 `pool`/`config`
  扩展）。现将 `pool` / `config` / `server` / `upstream` 四个文件纳入
  `PROTECT_FILES`，并在结束时对其做**上游差异检查**——上游若改动过会显式
  告警提示人工 merge，而不再静默跳过。
- 修正 `TestModelsDynamic` 的过期断言（v1.1.0b11 起 `/v1/models` 会追加虚拟
  `cheapest` 条目，测试仍按 3 个断言，已失败多时）。

## v1.1.0b12 (2026-09-07) 测试版

### 变更
- **cheapest 白名单勾选自动保存**：勾选/取消勾选后 1 秒自动写入配置（防抖合并
  连续勾选），不再依赖手动点保存按钮；「保存 cheapest 白名单」按钮保留作为
  立即触发。刷新/切页后勾选状态从配置恢复，不再丢失。

## v1.1.0b11 (2026-09-07) 测试版

### 新增
- **`/v1/models` 暴露虚拟 `cheapest` 模型**：每个已接入账号的平台在模型列表
  末尾追加 `<平台>/cheapest` 条目，客户端模型下拉框可直接选择，
  请求时解析为该渠道费率最低的模型（此前 cheapest 只能"隐式"请求，
  列表里看不到）。`auto` 仍为上游原生模型直通。

## v1.1.0b10 (2026-09-07) 测试版

### 变更
- **serverd 日志接入容器 stdout**：此前 Go 服务的日志被 DEVNULL 丢弃，
  排查路由/解析问题只能盲猜。现在 `ha app logs` / `docker logs` 可直接看到。

## v1.1.0b9 (2026-09-07) 测试版

### 变更
- **`auto` 更名 `cheapest`（破坏性）**：本地「选费率最低模型」功能与 WorkBuddy
  上游原生 `auto` 模型（平台智能路由）撞名，本地拦截导致上游 auto 永远调不到。
  现请求 `<平台>/cheapest` 走本地最低费率选择；`<平台>/auto` **直通上游**。
  已用过 `auto` 的客户端需改模型名。白名单配置项 `auto_models_*` 名称不变。
- **概览页 API Key 可一键复制**：接入说明的 API Key 行直接显示真实 key
  （概览接口有门禁：登录会话或本地来源），带「复制」按钮，不再让用户去设置页找。

### 修复
- **模型页勾选状态丢失**：此前勾选只存在 DOM 里，切换筛选/搜索触发表格重渲染
  后勾选全丢、保存的是残缺列表。现勾选即时写入内存白名单（`onchange`），
  重渲染从内存恢复，保存按钮只负责持久化。

## v1.1.0b8 (2026-09-07) 测试版

### 修复
- **disable/enable 路由遗漏**：b7 的 Go 端与前端按钮已就绪，但面板 POST 路由
  未加 `disable` / `enable` 分支导致 404。已补。

## v1.1.0b7 (2026-09-07) 测试版

### 修复
- **auto 勾选列始终显示**：b3 版本实现为「渠道已有白名单才显示勾选框」，
  导致未配置过白名单时模型页看不到任何勾选入口。现所有渠道恒显示勾选框，
  勾选后点「保存 auto 白名单」写入；全部取消勾选并保存即清空白名单（不限制）。

### 新增
- **账号手工禁用/启用**：账号管理页每个账号新增「禁用」按钮（带确认），
  禁用后暂停参与轮转（状态显示「已禁用」，原因「手工禁用」），可点「启用」恢复
  （同时清除冷却与低积分标记）。Go 端 `Pool.SetEnabled` + `ctl disable|enable`。
  与「解锁/解禁」（针对低积分/冷却）并存于操作列。

## v1.1.0b4 (2026-09-07) 测试版

### 安全加固（高危修复）
- **管理接口鉴权补全**：`do_POST` 此前完全没有登录门禁——公网访客可未登录调用
  `delete` / `config` / `unlock` 等写操作。现 GET/POST 管理接口统一走
  `_mgmt_authorized`：已登录会话放行；面板未启用登录（webui 凭据留空）时仅允许
  本机 / HA 内网来源（127.0.0.1、172.30/16、10/8、192.168/16），公网直连 7870
  返回 401。`/v1/*` API 不受影响，仍由 `api_key` 独立鉴权。
- **config 接口脱敏**：`GET /wb-api/config` 不再明文回显 `api_key`（仅前 4 位）
  与 `webui_pass`；保存端兼容掩码值（回传 `…` 结尾视为未修改，保留原 key）。
- **会话 Cookie 加固**：登录/登出改由后端 `Set-Cookie`，带 `HttpOnly; SameSite=Lax`
  （https 场景附 `Secure`），前端不再经 `document.cookie` 写 token。
- **登录防爆破**：同 IP 连续失败 5 次冷却 5 分钟（429）。

### 修复
- **ADDON_SLUG 硬编码错误**：原写死 `9a112f41_ai-proxy`（与实际安装 slug
  `74d83536_ai-proxy` 不符），导致 ingress 模式下前端拼接 API Base URL 错误。
  现运行时读容器环境变量 `SLUG`（HA 自动注入），本地开发回退 `ai-proxy`。
- **默认签到时间不一致**：`login_ui.py` 默认值 `09:00,21:00` 落后于 Go 端 /
  schema 的 `00:00,09:00,21:00`，经面板保存会丢 00:00 段、破坏次日自动解禁闭环。
  已统一。

### 文档
- README 更新当前版本与安全提示。

## v1.1.0b3 (2026-09-07) 测试版

### 界面重构（配置统一到 Web UI）
- **删除「费率」页**：费率信息与模型页重复（模型表本就带费率列），
  「刷新费率」按钮移入模型页工具栏。
- **auto 白名单改为模型页勾选**：模型表新增「auto」勾选列，按渠道勾选候选模型，
  点「保存 auto 白名单」写入配置（等价于 `auto_models_*`）。
  白名单非空的渠道可取消勾选并保存以清空（恢复不限制）；
  设置页原文本框移除，仅留指引。
- **账号解禁扩展**：冷却中的账号（429/错误/余额不足冷却）此前无手工恢复入口，
  现在与低积分一样显示按钮（低积分显示「解锁」、冷却显示「解禁」），
  复用 `ctl unlock`（永久禁用账号仍不可解）。

## v1.1.0b2 (2026-09-07) 测试版

### 新增
- **设置页纳入 auto 候选白名单**：Web UI「设置」页新增「auto 模型候选白名单」分组，
  `auto_models_workbuddy` / `auto_models_traework` / `auto_models_qoder` 三个字段
  可直接在面板编辑（此前只能在 HA 加载项配置里改），保存后随 serverd 热重启生效。
  至此全部运行配置均可在 Web UI 统一管理，HA 加载项配置页仅作初始值兜底。

## v1.1.0b1 (2026-09-05) 测试版

### 新增
- **`auto` 模型**：自动选择费率最低的模型。支持按渠道配置白名单
  （`workbuddy` / `traework` / `qoder` 独立的 `auto` 候选模型，见 `auto_model` 配置）。
  0 费率模型不参与 auto 竞争，避免选中按对话免费的特殊模型。
- **低积分账号管理**：账号积分低于 `low_credit_threshold`（默认 10）时，
  标记为「低积分」状态，仅允许使用 0 费率模型；可手工解锁。
- **免费额度耗尽自动禁用**：低积分账号调用 0 费率模型仍报余额不足时，
  判定为「免费额度已用完」，自动冷却到次日 0 点，次日签到后按新积分恢复。

### 修复
- **0 费率模型路由逻辑**（核心）：此前错误地「0 费率模型只路由到低积分账号」，
  导致无低积分账号时免费模型全部 `503`。现修正为：
  - 付费模型（费率>0 或未知）→ 仅高积分账号可用
  - 免费模型（费率==0）→ 所有健康账号可用，**优先消耗低积分账号，无低积分时回退高积分账号**
- **错误冷却判定依据**：由「依据模型费率」改为「依据账号实际低积分状态」，
  避免高积分账号调用免费模型余额不足被误判为「免费额度用完→次日」。
- **`unlock` 顺序**：先校验 `Disabled` 再解锁，避免对永久禁用账号做无效操作。

### 签到
- 默认签到时间增加 `00:00`（`config.go` / `scheduler.go` / `login_ui.py` / `config.yaml` 同步），
  确保「次日 0 点签到领取新积分 → 自动解禁」闭环。

## v1.0.5 (2026-08-30)

### 新增
- **WorkBuddy 流量统计「客户端」识别**：`ChatHeaders` 补发官方桌面端同款的客户端身份头
  `X-IDE-Type`/`X-IDE-Name`（= `WorkBuddy`）、`X-IDE-Version`、`X-Product-Version`，
  使上游用量统计界面把本代理发起的请求识别为 **workbuddy**（此前为空）。
  - 根因：WorkBuddy 用量统计的「客户端」列读取 `X-IDE-Type`/`X-IDE-Name`，
    上游 wild-work 从未发送这些头，故显示为空；官方桌面端默认 ideType/ideName = "WorkBuddy"。
  - 依据：直接解包官方 WorkBuddy 桌面端 `app.asar` + `app.asar.unpacked/cli/dist/codebuddy.js`，
    确认桌面端与 CLI 的头契约（桌面 `WorkBuddy`，CLI `CLI`）。
  - `internal/upstream/headers.go` 已加入 `sync_vendor.sh` 的 `PROTECT_FILES`，
    该文件转为 addon 独有维护，`sync_vendor.sh` 不再覆盖。
- 版本升至 `1.0.5`（`config.yaml` / `build.yaml`），高于 HA 当前运行的 `1.0.4`，供 Supervisor 升级。

## v1.0.1 (2026-08-30)

### 修复
- **恢复缺失的积分相关接口**（根因：addon 对齐 `ad32896`，落后上游 `c62d0bc`）
  - `traework` 补 `FetchModelPricing`、`UserResourceDetail`、`parseTraeFeatures`
  - `upstream`（WorkBuddy）补 `UserResourceDetail`、`FetchModelPricing`、`parseCredits`
  - `qoder` 补 `UserResourceDetail`、`FetchModelPricing`
  - `provider.Upstream` 接口升至 10 方法（新增上述两项）
  - 此前是**静默降级**：接口仍是旧版 8 方法，Client 不实现也不报错，
    表现为"积分功能不可用"但无错误信息
- **TraeWork 登录（部分修正，仍有未验证项）**：去掉公网回调的 `/authorize` 后缀
  （面板端点就是 `/api/trae-cb`，加后缀会 404），`redirect` 改 `0`；面板登录后
  自动轮询完成，无需手动复制回调链接
  - 实测确认 **refreshToken 直登可用**（旧/新二进制用同一真实 token 均成功）
  - 公网回调 + authcode 交换路径**尚未端到端验证**，若授权链接登录失败应优先用
    refreshToken 直登

### 工程
- 精准同步上游 4 个包（`traework`/`upstream`/`qoder`/`provider`），
  `pool`/`server`/`scheduler` 暂不同步以缩小变更面
- **修复上游 `c62d0bc` 的语法错误**：补回 `UserEntUsage` 中被误删的
  `var resp struct {`（上游该提交自身无法 `go build`）
- `go build ./...`、`go vet ./internal/...` 通过；
  linux/arm64 + amd64 五个二进制交叉编译通过
- 新增 `UPSTREAM-GAP-ANALYSIS.md`（差距分析、协议要点、后续风险）

## v1.0.0 (2026-08-26)

首个稳定发布。

### 功能
- WorkBuddy + TraeWork(SOLO) + Qoder 多账号聚合为 OpenAI 兼容 API
- 模型名前缀路由（`workbuddy/<model>` / `traework/<model>` / `qoder/<model>`）
- 自动签到、多账号轮转、粘性路由（提升会话缓存利用率）
- TraeWork 登录：公网回调（`/api/trae-cb`）+ refreshToken 直登兜底，适配异地 HA 容器
- 管理 Web UI（`login_ui.py`，ingress 7863 统一入口）

### 工程
- 上游 `wild-work` 复制式同步机制（`SYNC.md` + `sync_vendor.sh` + `Makefile`）
  - 对齐上游 commit ad32896
  - addon 独有登录逻辑隔离在 `internal/login_trae/addon_extras.go`，不被同步覆盖
  - 生成脚本自动补全 module 路径（`wild-work/internal` → `github.com/rockswang/workbuddy-wild/internal`）
- `go build ./...` + `go vet ./...` 通过；5 个二进制（serverd/ctl/login/logintrae/loginqoder）linux 交叉编译验证
