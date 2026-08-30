# Changelog

## v1.0.1 (2026-08-30)

### 修复
- **恢复缺失的积分相关接口**（根因：addon 对齐 `ad32896`，落后上游 `c62d0bc`）
  - `traework` 补 `FetchModelPricing`、`UserResourceDetail`、`parseTraeFeatures`
  - `upstream`（WorkBuddy）补 `UserResourceDetail`、`FetchModelPricing`、`parseCredits`
  - `qoder` 补 `UserResourceDetail`、`FetchModelPricing`
  - `provider.Upstream` 接口升至 10 方法（新增上述两项）
  - 此前是**静默降级**：接口仍是旧版 8 方法，Client 不实现也不报错，
    表现为"积分功能不可用"但无错误信息
- **TraeWork 登录回调修正**：去掉公网回调的 `/authorize` 后缀（面板端点就是
  `/api/trae-cb`，加后缀会 404），`redirect` 改 `0`；面板登录后自动轮询完成，
  无需手动复制回调链接

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
