// CODE GENERATED FROM wild-work@c62d0bc -- DO NOT EDIT, run sync_vendor.sh

// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"fmt"
	"io"
	"net/http"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// Kind 平台标识，同时也是模型名前缀。
type Kind string

const (
	WorkBuddy Kind = "workbuddy"
	TraeWork  Kind = "traework"
	Qoder     Kind = "qoder"
)

func (k Kind) String() string { return string(k) }

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额/权益不足 → 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 登录态失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却不累计 errCount
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误（默认兜底）

	// ── 「与账号无关」的错误：不应罚账号 ──
	// 依据：把「请求本身的问题」与「账号的问题」分开，避免把健康账号误冷却，
	// 或对确定无解的请求反复轮转、白白消耗其他账号的配额。
	// （对齐上游 wb2api 的 errorRule 策略）
	ErrContentBlocked // 内容策略拦截（400 + 审核文案）→ 不罚账号
	ErrPromptTooLong  // 上下文超限（11115）→ 请求级错误，不罚号不轮转
	ErrBadParams      // 请求体解析失败（11101）→ 不罚号，但仍轮转

	// ── 账号级故障：需冷却或禁用 ──
	ErrWafBlock     // 403 + 非业务信封（WAF 拦截页/空体）→ 账号软冷却
	ErrAccountFault // 账号级授权/配额故障（11140 / 14017）→ 冷却轮换
	ErrModelBlocked // 11102 该后端无此模型 → 按 (账号,模型) 避让

	// ErrPassthrough 请求级错误：透传原文给客户端，**不冷却账号**。
	// 用于「上游按模型限流」（issue #53）：此时账号本身健康，
	// 只是当前模型暂时不可用；若按账号级 ErrSoftRate 冷却，
	// 会把该账号其它还能用的模型一起拖进冷却。
	ErrPassthrough
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrBadParams:
		return "bad_params"
	case ErrWafBlock:
		return "waf_block"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrPassthrough:
		return "passthrough"
	default:
		return "none"
	}
}

// PenalizesAccount 报告该错误是否应影响账号健康度（冷却/错误计数/熔断）。
//
// 「不罚账号」的三类都是**请求本身的问题**，换任何账号结果相同：
//   - ErrContentBlocked：上游按逐字指纹审核，system 来源的模板句触发误报
//   - ErrPromptTooLong：上下文超限，与账号无关
//   - ErrBadParams：发出去的 body 有问题
//
// 对它们调用 NoteError 会把健康账号冷却掉，并浪费其他账号的请求配额。
func (k ErrKind) PenalizesAccount() bool {
	switch k {
	case ErrContentBlocked, ErrPromptTooLong, ErrBadParams, ErrPassthrough:
		return false
	default:
		return true
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// ModelInfo 动态/静态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// ModelPricing 模型积分定价（从上游 API 拉取）。
type ModelPricing struct {
	Model   string  `json:"model"`
	Channel string  `json:"channel"`
	Rate    float64 `json:"rate"`
	Note    string  `json:"note,omitempty"` // 折扣/标签说明
}

// Upstream 是 server/scheduler 依赖的最小上游能力集合。
type Upstream interface {
	RefreshToken(a *auth.Auth) error
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	FetchModelPricing(a *auth.Auth) ([]ModelPricing, error)
	UserResource(a *auth.Auth) (int64, error)
	UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind
	Stream(w http.ResponseWriter, r io.Reader) error
	Aggregate(r io.Reader) (map[string]any, error)
}

// ResourceItem 积分明细条目。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`

	// ── 以下字段供 ledger 差分对账与「过期/消耗」归因 ──
	//
	// 背景：此前本地只有上面 4 个字段，双流水账本（internal/ledger）因此
	// **无法区分「积分包到期作废」与「正常消耗」** —— 而能区分这两者正是
	// ledger 与 stats 刻意不合并的根本理由（上游 R43）。ledger 包内一度用
	// provider_compat.go 复刻这些字段绕过；这里补齐到源头。
	//
	// 新增字段零值安全，旧调用方不填也能正常编译运行。

	// ExpireAt 该条目到期时刻，YYYY-MM-DD（UTC+8 墙钟）。
	// 空串表示上游未下发到期时间 —— 不得用零值时间冒充「永不过期」。
	ExpireAt string `json:"expire_at,omitempty"`
	// Key 条目稳定标识（上游 ID，如 TraeWork entitlement_id），
	// 供 ledger 差分对账用；渠道无 ID 时留空，差分退回 Name 作伪键。
	Key string `json:"key,omitempty"`
	// Usable 是否属于本工具可消耗的额度池。
	// 零值为 false，故各渠道构造时须显式置位；渠道无此概念时统一填 true。
	Usable bool `json:"usable"`
	// InfoOnly 只作展示，不参与任何积分算术。
	InfoOnly bool `json:"info_only,omitempty"`
}

// Rotatable 报告该错误是否应【换下一个账号重试本请求】（账号级错误）。
//
// 移植自上游 wild-work PR #82（d4314a2），但按本项目更细的 ErrKind 语义调整：
//
//	账号有问题的（限流/欠费/登录态死/404/5xx/WAF/账号故障/模型不存在/业务错误）
//	  → 换号有价值，重试本请求；
//	请求内容有问题的（内容拦截 / 上下文超限 / 请求体解析失败）
//	  → 换任何账号都必然复现，应【立刻透传原文】结束，
//	    否则会白白轮转、消耗其他账号配额，且客户端会收到本可避免的 503。
//
// 与 PenalizesAccount 的关系：两者互补，判定对象不同
//   - PenalizesAccount：决定是否【罚账号】（冷却/禁用/计数）
//   - Rotatable：决定是否【换号重试】
//
// 注意 ErrBadParams：其定义注释为"不罚号，但仍轮转"，
// 故这里返回 true（与 PromptTooLong"不轮转"形成对照）。
func (k ErrKind) Rotatable() bool {
	switch k {
	case ErrContentBlocked, ErrPromptTooLong:
		// 请求本身的问题：换号必然复现 → 不换号，透传原文
		return false
	case ErrSoftRate, ErrHardCredit, ErrSessionDead, ErrNotFound,
		ErrServer, ErrClient, ErrWafBlock, ErrAccountFault,
		ErrModelBlocked, ErrBadParams, ErrPassthrough:
		return true
	default:
		return true
	}
}
