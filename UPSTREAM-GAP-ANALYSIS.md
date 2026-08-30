# 上游对齐差距分析（2026-08-30）

> 目的：解释 addon 当前两个现象的根因，并记录"继续研究上游实现方式"的结论。
> 上游：`rockswang/wild-work`，本地克隆 `D:/ai-hub/_upstream-wild-work`。

## 结论摘要

addon 当前对齐上游 **`ad32896`**，上游 HEAD 已到 **`c62d0bc`**（领先 2 个提交）。
**addon 在三个渠道包都缺同一组"积分"相关函数**，这正是：

- "WorkBuddy 积分无法使用" 的根因
- "TraeWork 客户端能用 / addon 侧不能用" 的根因（官方客户端不依赖本仓库代码）

## 上游新增的 2 个提交

| commit | 内容 | 对 addon 的影响 |
|--------|------|----------------|
| `abb5875` | 修复批量操作 404：`refresh_all`/`checkin_all` 需传 body 触发 POST（Go 1.22+ ServeMux 严格匹配方法） | 仅改 `cmd/wild-work/web/app.js`（桌面前端），**addon 面板不涉及** |
| `c62d0bc` | **TraeWork 积分明细显示优化：按 `group_name` 分组** + 新增 560 行逆向文档 | 改 `internal/traework/client.go`，**addon 需同步** |

> 关键：上游还新增了 `docs/upstream-reverse-engineering.md`（560 行），
> 完整记录 WorkBuddy / TraeWork / Qoder 三家的登录、积分、签到协议与错误码。
> **这份文档是本次"研究上游实现方式"最重要的资料。**

## addon vs 上游：逐包缺失函数

| 包 | 上游 | addon | addon 缺失 |
|----|------|-------|-----------|
| `traework` | 50 | 47 | **`FetchModelPricing`**, **`UserResourceDetail`**, `parseTraeFeatures` |
| `upstream`（WorkBuddy） | 29 | 26 | **`UserResourceDetail`**, **`FetchModelPricing`**, `parseCredits` |
| `qoder` | 47 | 45 | **`UserResourceDetail`**, **`FetchModelPricing`** |
| `pool` | 22 | 21 | `SetDisabled` |
| 其余包 | — | — | 无缺失 |

### 缺失函数的作用

| 函数 | 作用 | 缺失后果 |
|------|------|---------|
| `FetchModelPricing` | 拉模型 **`consumption_rate.rate`（积分倍率）** | 不知道"某模型消耗多少积分"→ 积分计量/展示不可用 |
| `UserResourceDetail` | 查 **积分明细**（每套餐条目：总额/已用/剩余） | 面板看不到积分来源与余额明细 |
| `parseCredits` / `parseTraeFeatures` | 解析积分/倍率字段 | 上述两者依赖 |
| `pool.SetDisabled` | 账号停用/启用 | 无法在池层禁用账号 |

## 为什么 addon 还能编译（接口被"降级"）

上游 `provider.Upstream` 接口已是 **10 个方法**（新增 `FetchModelPricing`、`UserResourceDetail`），
而 addon 的 `internal/provider/provider.go` 仍是**旧版 8 方法**。
因此三个渠道 Client 即使没实现这两个方法也能编译——**但积分能力整体缺失**。
这是"静默降级"：不报错，只是功能没有。

## 上游协议要点（摘自 upstream-reverse-engineering.md）

### TraeWork 登录（协议层未变，addon 已适配）
- 换 token：`POST {api.trae.cn}/trae/api/v3/oauth/ExchangeToken`
  body: `{"ClientID":"solo_agent_web","RefreshToken":"<rt>","ClientSecret":"-","UserID":""}`
- 鉴权头：`Cloud-IDE-JWT <accessToken>`
- 桌面版监听 `127.0.0.1:0`，回调 `http://127.0.0.1:<port>/authorize`
- addon 用 `internal/login_trae/addon_extras.go` 改为**公网回调** + `CompleteRefresh`（refreshToken 直登）

### WorkBuddy 资源（addon 缺明细）
- `POST {billing}/v2/billing/meter/get-user-resource`
- 余额优先级：`CycleCapacitySize>0` → `CycleCapacityRemain`；否则 `CapacityRemain`
- addon **有** `UserResource`（总余额），**缺** `UserResourceDetail`（明细）

### TraeWork 积分明细（c62d0bc 优化点）
- `POST /trae/api/v2/pay/web_user_ent_usage`
- 条目名优先 `group_name`（"每日签到"、"每月登录积分"），其次 `display_desc`，
  最后兜底 `package_name`/`group_type`——解决旧版"一水套餐分不清来源"

## 修复方案

1. **同步上游**：`cd src && ./sync_vendor.sh`
2. **确认保护文件未被覆盖**：`internal/login_trae/addon_extras.go`、`internal/svc/svc.go`
3. 同步后 `provider.Upstream` 自然变 10 方法，三渠道 Client 满足接口
4. **编译验证**：`go build ./... && go vet ./internal/...`
5. **回归**：面板积分明细、模型倍率、TraeWork refreshToken 直登

## 风险与注意事项

- **`sync_vendor.sh` 的 PROTECT_FILES 匹配可疑**：
  `case "${dst}" in *"${PROTECT_FILES// /\n}"* )` 把换行塞进 case 模式，
  **很可能匹配失败** → `addon_extras.go`、`svc.go` 有被上游同名文件覆盖的风险。
  同步后必须校验这两个文件仍存在且内容未变。
- 上游 `abb5875` 改的是桌面前端 `app.js`；addon 面板是 `login_ui.py`（Python），
  若面板有"批量刷新/批量签到"入口，需**另外对照实现** POST 传 body。
