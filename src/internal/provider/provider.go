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
	case ErrContentBlocked, ErrPromptTooLong, ErrBadParams:
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
}
