package upstream

import (
	"net/http"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// ── issue #53：上游按模型限流不得冷却整个账号 ──────────────────

// TestClassifyModelLevelRateLimitIsPassthrough —— 核心回归（issue #53）。
//
// 上游按**模型**限流时（body 含 "switch to the other models"），
// 该账号其它模型仍能正常服务。若归为 ErrSoftRate 会冷却整个账号，
// 把其它可用模型一起拖进冷却 —— 这正是 #53 要解决的问题。
func TestClassifyModelLevelRateLimitIsPassthrough(t *testing.T) {
	cases := []string{
		`{"code":6004,"msg":"Rate limit reached, please switch to the other models"}`,
		`switch to the other models`,
		`{"msg":"usage exceeds frequency limit, switch to the other models"}`,
	}
	for _, body := range cases {
		got := Classify(http.StatusTooManyRequests, body)
		if got != ErrPassthrough {
			t.Errorf("Classify(429,%q)=%v want ErrPassthrough", body, got)
		}
	}
}

// TestClassifyNormalRateLimitStillSoftRate —— 普通 429（账号级限流）
// 必须仍判 ErrSoftRate，不能被 #53 的判据误吞。
func TestClassifyNormalRateLimitStillSoftRate(t *testing.T) {
	cases := []string{
		``,
		`{"msg":"rate limit exceeded"}`,
		`{"code":6004,"msg":"rate limit"}`,
		`{"msg":"The model provider is rate-limiting requests."}`,
	}
	for _, body := range cases {
		got := Classify(http.StatusTooManyRequests, body)
		if got != ErrSoftRate {
			t.Errorf("Classify(429,%q)=%v want ErrSoftRate（普通限流应保持账号级）", body, got)
		}
	}
}

// TestErrPassthroughDoesNotPenalizeAccount —— 语义锁定。
func TestErrPassthroughDoesNotPenalizeAccount(t *testing.T) {
	if provider.ErrPassthrough.PenalizesAccount() {
		t.Error("ErrPassthrough 不应罚账号（账号其它模型仍可用）")
	}
	if !provider.ErrSoftRate.PenalizesAccount() {
		t.Error("ErrSoftRate 应罚账号（账号级限流确实需要冷却）")
	}
	if got := provider.ErrPassthrough.String(); got != "passthrough" {
		t.Errorf("String()=%q want passthrough", got)
	}
}
