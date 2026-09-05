# Changelog

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
