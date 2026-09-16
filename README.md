<img width="750" height="1110" alt="image" src="https://github.com/user-attachments/assets/4f077501-11cc-4947-9e0f09dedf98" /># AI Proxy — 多平台 AI 账号 OpenAI 兼容代理（HA Add-on）

把 **WorkBuddy / CodeBuddy + TraeWork(SOLO) + Qoder** 多账号聚合成 OpenAI 兼容 API，
模型名加来源前缀自动路由；支持自动签到、多账号轮转、粘性路由。

> 上游协议层来自 [wild-work](https://github.com/rockswang/wild-work)（同步基线
> `c62d0bc`；详见 `UPSTREAM-GAP-ANALYSIS.md`），本 addon 在其基础上做
> **异地 HA 容器适配**（公网回调登录 + refreshToken 直登兜底）。

<p align="center">
  <a href="https://www.workbuddy.cn/events/invite?inviteCode=r6pi8bgu2">
    <img width="750" alt="WorkBuddy 注册邀请" src="https://github.com/user-attachments/assets/b1208e4a-b0f8-4cff-96e3-bf291e3ae6d8" />
  </a>
  <br />
  WorkBuddy 注册邀请：<a href="https://www.workbuddy.cn/events/invite?inviteCode=r6pi8bgu2">https://www.workbuddy.cn/events/invite?inviteCode=r6pi8bgu2</a>
</p>

## 功能

- OpenAI 兼容 `/v1/chat/completions`、`/v1/models`
- 模型前缀路由：`workbuddy/<model>` / `traework/<model>` / `qoder/<model>`
- 多账号轮转 + 粘性路由（连续 50 次成功或遇错才换号，提升会话缓存复用）
- 自动签到、token 保活
- **TraeWork 登录**：公网回调 `/api/trae-cb` + refreshToken 直登兜底（适配无本机浏览器的容器）

## 安装（Home Assistant）

1. 把本仓库作为自定义 add-on 仓库添加，或直接把本目录放到 `/addons/` 下
2. 在 HA → 加载项 中找到 **AI Proxy** → 安装
   - 镜像由 CI 预构建推送至 `ghcr.io/c3h3-ai/ai-proxy`，**安装时直接拉取，无需本地编译**
3. 配置（`config.yaml` 同款字段）：
   - `api_key`：OpenAI API 鉴权（空=不鉴权）
   - `region`：`cn` / `global`
   - 轮转冷却：`cooldown_hard_credit` / `cooldown_soft_rate` / `cooldown_err_threshold` / `cooldown_err_cooldown`
   - `checkin_times`：每日签到时间（如 `09:00,21:00`）
4. 启动 → 打开 Web UI（ingress 7870）→ 添加账号

## ⚠️ 安全：面板登录与访问来源

**强烈建议在面板「设置」页配置 WebUI 登录账号/密码。**

未配置时，管理接口的访问规则如下：

| 访问方式 | 未配面板账号 | 已配面板账号 |
|---|---|---|
| HA 侧边栏（ingress） | ✅ 放行 | 需登录 |
| 容器内 / 本机回环 | ✅ 放行 | 需登录 |
| **局域网 IP 直连 7870** | ❌ **拒绝** | 需登录 |

> 之所以不信任整个内网：ingress 转发（`172.30.x`）与局域网直连
> （`192.168.x`）的**源 IP 同属内网，仅凭 IP 无法区分**。若默许内网放行，
> 同网段任意设备即可无认证调用管理接口（账号列表、刷新令牌、修改配置）。
> 而 add-on 的 `7870/tcp` 映射到宿主机，该风险是实际可达的。

**被拦截时的解决办法（任选其一）**：

1. 在设置页配置面板登录账号/密码 —— 之后任何来源均可正常登录访问；
2. 改从 HA 侧边栏（ingress）进入面板。

> **`/v1/*` OpenAI API 不受影响** —— 它由 `api_key` 独立鉴权
> （面板层与 serverd 层各校验一次），与会话登录无关，端口相同但鉴权链独立。

## TraeWork 登录（容器适配要点）

- **公网回调**：面板生成授权链接，回调指向 `http://<ha>:7870/api/trae-cb`（经 ingress 可达容器）
- **refreshToken 直登（最稳）**：在面板粘贴 refreshToken 直接换 token，无需浏览器回调
- 不要相信"授权页硬性绑定 127.0.0.1"的说法——真实抓包证明公网/localhost 回调均可交付 token

## 开发者：上游同步

上游 `wild-work` 把核心包放 `internal/`（Go 语言级封锁，外部 module 不能 import），
本 addon 采用**复制式同步 + 适配层隔离**：

```bash
cd src
UPSTREAM_DIR=/path/to/wild-work ./sync_vendor.sh   # 生成同源 internal 包（自动补全 module 路径）
make build                                          # 编译
make vet                                            # 静态检查
```

`sync_vendor.sh` 的行为：

1. 复制上游 `internal/{auth,config,login,...,traework,upstream}` 到 addon
2. 每个生成文件顶部打 `CODE GENERATED FROM wild-work@<commit>` 头
3. **跳过 `PROTECT_FILES`** —— addon 独有文件 + **addon 修改过的上游文件**
4. 结束时对受保护文件做**上游差异检查**：上游若改动过会打 `[WARN]` 提示人工 merge

> ⚠️ **跑 sync 前请确认工作区干净**（`git status` 无未提交改动）——
> 它会直接覆盖 20+ 个文件。

### 受保护文件（不会被 sync 覆盖）

| 文件 | 原因 |
|---|---|
| `login_trae/addon_extras.go`、`svc/svc.go` | addon 独有，上游没有 |
| **`pool/pool.go`** | 改了 `Pick()`/`pickExcluding()` 内部实现。Go **不支持方法覆写**，且所需状态是**私有字段**，外部包访问不了 |
| **`config/config.go`** | `LowCreditThreshold` 是 `config.Config` 的字段 |
| **`server/handler.go`** | 改 `chatCompletions` 内部逻辑 |
| **`upstream/client.go`** | 改 `Client` 结构体与方法 |
| `upstream/headers.go`、`traework/constants.go` | addon 适配 |

**代价**：这些文件不再跟随上游自动更新，需人工 merge。
**目前代价为零**：上游自基线起对这四个包的改动提交数为 0。

包归属、同步命令、本地验证详见 `SYNC.md`；完整的模块职责与数据流见 `TECHNICAL-DOC.md`。

## 构建与发版

### 镜像（自动）

多架构（amd64 / aarch64）镜像由 GitHub Actions 用 HA 官方 builder actions
预构建并推送到 `ghcr.io/c3h3-ai/ai-proxy`，见 `.github/workflows/build-image.yml`。
Supervisor 直接拉镜像，不在用户设备上编译。

### CI 门禁

| workflow | 作用 |
|---|---|
| `go-ci` | `go build` / `go vet` / `go test`（仅 `src/**`、`Dockerfile` 变更时触发） |
| `pr-validate` | 分支来源、标题格式、描述完整性、变更类型唯一 |
| `pr-label` | 按 PR 模板勾选自动打标（供 release notes 分类） |
| `build-image` | 构建并推送多架构镜像，末尾校验镜像可达 |
| `release` | 读 `config.yaml` 版本 → semver 比较 → 建 tag → 建**草稿** release |
| `stale` / `sync-labels` | issue 清理 / 标签同步 |

### 发版流程

```
1. 改 config.yaml 的 version（必须【严格大于】最新 tag，否则 release 会跳过）
2. 合并到 master
3. CI 自动：建 tag → 建草稿 release → 构建并推送镜像
4. 维护者 review 草稿 → 手动发布
```

> ⚠️ **改了 `src/**` 就应当 bump `config.yaml` 的版本。**
> 否则 `build-image` 会用**旧版本号**覆盖同名镜像 tag，
> 造成「镜像内容」与「代码版本」脱节（`v1.1.0b13` 曾出现此情况）。

### 本地验证

```bash
export PATH="$PATH:/path/to/go/bin"   # 若 Go 不在 PATH
cd src
GOPROXY=https://goproxy.cn,direct GOSUMDB=off go build ./... && go vet ./... && go test ./...
```

面板相关改动另有两个校验脚本（不依赖服务启动）：

```bash
python3 scripts/test_mgmt_auth.py       # 管理接口鉴权真值表
python3 scripts/test_session_token.py   # 会话 token 生成与比较
```

> `build.yaml` 已移除：它是 legacy builder 的配置，自 Supervisor 2026.04.0 起
> **不再被读取**。基础镜像由 Dockerfile 的 `FROM` 决定，`BUILD_VERSION` /
> `BUILD_ARCH` 由 Supervisor 按 `config.yaml` 的 `version` 自动注入。

## 版本

当前 `1.1.0b14`。详见 `CHANGELOG.md`。

> ⚠️ 另一个仓库 `C3H3-AI/ai-proxy-test` 提供的是 `2.0.1` 构建，**落后于本仓库**——
> 它停在 2026-08-27，不含 TraeWork 通道修复（4008）、WorkBuddy 客户端识别头、
> 面板登录鉴权等 8/30 之后的改动，且 addon 位于 `ai-proxy/` 子目录、
> 构建上下文与本仓库不同（`COPY . /src` vs `COPY src /src`）。
>
> 若 HA 里添加过该仓库并看到 `2.0.1` 更新提示，请勿升级，以免功能回退。
> 本仓库（addon 位于根目录）才是持续维护的版本。
