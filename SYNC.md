# SYNC — 上游同步基线

本 addon 的 Go 核心代码 **vendor 复制**自上游 `rockswang/wild-work`（桌面 daemon 版）。
由于上游把核心包放在 `internal/` 下（Go 语言级封锁，外部 module 不可 import），
addon 采用「复制 + 生成式 sync + 适配层隔离」策略，而非 Go module 依赖。

## 对齐基线（重要：上游更新时先核对这个 commit）

| 项 | 值 |
|---|---|
| 上游仓库 | https://github.com/rockswang/wild-work |
| 本地克隆 | `D:/ai-hub/_upstream-wild-work` |
| **对齐 commit** | `c62d0bc` （TraeWork 积分明细按 group_name 分组） |
| 对齐日期 | 2026-08-30（本次同步 4 个包：traework/upstream/qoder/provider） |

> 上游 Trae 登录相关文件（`login_trae/*`、`traework/*`）自 `13e7b64` "wild-work v2" 后**未再改动**。
> 最近活跃区是 `pool` / `server` / `qoder`（性能与透传优化），以及 CI / 文档 / `--no-tray`。


> ⚠️ **上游 `c62d0bc` 自身编译失败**：该提交在重构 `traework.UserResourceDetail` 时
> 误删了 `UserEntUsage` 里的 `var resp struct {`，导致上游仓库 `go build` 直接报
> `internal/traework/client.go:469:28: syntax error`。
> 本 addon 同步时已补回该行（见 `internal/traework/client.go`）。若后续上游修复，
> 重新 sync 时请确认这一行仍存在。

## 包归属表（谁负责 sync，谁改了要重编）

| 包 | 来源 | 更新策略 |
|---|---|---|
| `auth` `config` `login` `login_qoder` `login_trae` `pool` `provider` `qoder` `scheduler` `server` `traework` `upstream` | 上游 | **sync 生成**，不带手写改动（见 GENERATED 头） |
| `svc` | addon 独有 | 自行维护（HA 加载 `trae-*.json`） |
| `trae_adaptor` | addon 独有 | 自行维护（公网回调 + refreshToken 兜底，桌面版没有） |

## 上游更新时怎么做（一次性核对）

```bash
cd D:/ai-hub/_upstream-wild-work
git fetch && git log --oneline OLD_COMMIT..HEAD
# 只看 addon 也用的文件是否变了：
git log --oneline OLD_COMMIT..HEAD -- \
  internal/login_trae internal/traework internal/pool \
  internal/server internal/provider internal/qoder \
  internal/auth internal/config internal/login internal/scheduler internal/upstream internal/login_qoder
```

- 若上面**无输出** → 上游这次更新与 addon 无关，**无需追、无需重编**。
- 若有输出 → 跑 `sync_vendor.sh` 重新生成，再 `go build` 验证。

## sync 命令

```bash
cd D:/ai-hub/integrations/ha-ai-proxy/src
./sync_vendor.sh          # 从 _upstream-wild-work 当前 HEAD 生成 internal/* 同源包
```

生成脚本会：
1. 复制上游 `internal/{auth,config,login,login_qoder,login_trae,pool,provider,qoder,scheduler,server,traework,upstream}` 到 addon `src/internal/`
2. 在每个生成文件顶部打 `// CODE GENERATED FROM wild-work@<commit> — DO NOT EDIT, run sync_vendor.sh`
3. **不动** `svc` 和 `trae_adaptor`（addon 独有）
4. 输出变更文件列表供 review

## 本地验证编译（实测可用）

本机 Go 为便携版，不在默认 PATH，需先加路径（按实际安装位置调整）：

```bash
export PATH="$PATH:/c/Users/duola/go-portable/go/bin"   # 便携 Go 位置
cd D:/ai-hub/integrations/ha-ai-proxy/src
go build ./... && go vet ./...
```

依赖拉取：默认 proxy.golang.org 在受限网络不可达，改用国内代理：

```bash
GOPROXY=https://goproxy.cn,direct GOSUMDB=off go build ./...
```

> 注意：上游 `wild-work` 的 module 名是裸 `wild-work`，其 `internal/*` 包 import 路径为
> `wild-work/internal/...`。addon 的 module 是 `github.com/rockswang/workbuddy-wild`，
> 因此 `sync_vendor.sh` 在生成时会自动把 `wild-work/internal` 改写为
> `github.com/rockswang/workbuddy-wild/internal`（含完整前缀，否则会报 "not in std"）。

## 重编（HA addon 构建）

```bash
cd D:/ai-hub/integrations/ha-ai-proxy/src
go build ./... && go vet ./internal/...
```

构建由 HA addon 的 `build.yaml` + `Dockerfile` 多架构自动化完成。
