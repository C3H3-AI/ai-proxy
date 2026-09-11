<img width="750" height="1110" alt="image" src="https://github.com/user-attachments/assets/4f077501-11cc-4947-9fc8-9e0f09dedf98" /># AI Proxy — 多平台 AI 账号 OpenAI 兼容代理（HA Add-on）

把 **WorkBuddy / CodeBuddy + TraeWork(SOLO) + Qoder** 多账号聚合成 OpenAI 兼容 API，
模型名加来源前缀自动路由；支持自动签到、多账号轮转、粘性路由。
workbuddy注册邀请<img width="750" height="1110" alt="image" src="https://github.com/user-attachments/assets/b1208e4a-b0f8-4cff-96e3-bf291e3ae6d8" />https://www.workbuddy.cn/events/invite?inviteCode=r6pi8bgu2

> 上游协议层来自 [wild-work](https://github.com/rockswang/wild-work)（基线 commit `ad32896`，
> 积分相关接口已同步至 `c62d0bc`；详见 `UPSTREAM-GAP-ANALYSIS.md`），
> 本 addon 在其基础上做**异地 HA 容器适配**（公网回调登录 + refreshToken 直登兜底）。

## 功能

- OpenAI 兼容 `/v1/chat/completions`、`/v1/models`
- 模型前缀路由：`workbuddy/<model>` / `traework/<model>` / `qoder/<model>`
- 多账号轮转 + 粘性路由（连续 50 次成功或遇错才换号，提升会话缓存复用）
- 自动签到、token 保活
- **TraeWork 登录**：公网回调 `/api/trae-cb` + refreshToken 直登兜底（适配无本机浏览器的容器）

## 安装（Home Assistant）

1. 把本仓库作为自定义 add-on 仓库添加，或直接把本目录放到 `/addons/` 下
2. 在 HA → 加载项 中找到 **AI Proxy** → 安装
3. 配置（`config.yaml` 同款字段）：
   - `api_key`：OpenAI API 鉴权（空=不鉴权）
   - `region`：`cn` / `global`
   - 轮转冷却：`cooldown_hard_credit` / `cooldown_soft_rate` / `cooldown_err_threshold` / `cooldown_err_cooldown`
   - `checkin_times`：每日签到时间（如 `09:00,21:00`）
4. 启动 → 打开 Web UI（ingress 7870）→ 添加账号

## TraeWork 登录（容器适配要点）

- **公网回调**：面板生成授权链接，回调指向 `http://<ha>:7870/api/trae-cb`（经 ingress 可达容器）
- **refreshToken 直登（最稳）**：在面板粘贴 refreshToken 直接换 token，无需浏览器回调
- 不要相信"授权页硬性绑定 127.0.0.1"的说法——真实抓包证明公网/localhost 回调均可交付 token

## 开发者：上游同步

上游 `wild-work` 把核心包放 `internal/`（Go 语言级封锁，外部 module 不能 import），
本 addon 采用**复制式同步 + 适配层隔离**：

```bash
cd src
./sync_vendor.sh        # 从上游固定 commit 生成同源 internal 包（自动补全 module 路径）
make build              # 编译
make vet                # 静态检查
```

- 同源包（`auth`/`traework`/`pool`/`server`…）由 `sync_vendor.sh` 生成，带 `CODE GENERATED` 头
- addon 独有逻辑在 `internal/login_trae/addon_extras.go`（不被同步覆盖）
- 同步基线、包归属、本地验证见 `SYNC.md`

## 构建

多架构（amd64 / aarch64）由 HA add-on 构建系统按 `build.yaml` + `Dockerfile` 自动完成。
本地验证：

```bash
export PATH="$PATH:/path/to/go/bin"   # 若 Go 不在 PATH
cd src
GOPROXY=https://goproxy.cn,direct GOSUMDB=off go build ./... && go vet ./...
```

## 版本

当前 `v1.0.5`。详见 `CHANGELOG.md`。

> ⚠️ 另一个仓库 `C3H3-AI/ai-proxy-test` 提供的是 `2.0.1` 构建，**落后于本仓库**——
> 它停在 2026-08-27，不含 TraeWork 通道修复（4008）、WorkBuddy 客户端识别头、
> 面板登录鉴权等 8/30 之后的改动，且 addon 位于 `ai-proxy/` 子目录、
> 构建上下文与本仓库不同（`COPY . /src` vs `COPY src /src`）。
>
> 若 HA 里添加过该仓库并看到 `2.0.1` 更新提示，请勿升级，以免功能回退。
> 本仓库（addon 位于根目录）才是持续维护的版本。
