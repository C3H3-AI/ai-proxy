package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// newTestClient 构造指向测试服务端的 Client。
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := New()
	c.ChatBaseCN = srv.URL
	return c, srv
}

// TestRefreshSetsExpiryFromExpiresIn —— 上游给了 expiresIn 时按它设置。
func TestRefreshSetsExpiryFromExpiresIn(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`))
	})
	defer srv.Close()

	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	want := before + 3600
	if a.ExpiresAt < want-5 || a.ExpiresAt > want+5 {
		t.Errorf("ExpiresAt=%d want≈%d", a.ExpiresAt, want)
	}
	if a.AccessToken != "new-at" {
		t.Errorf("AccessToken=%q", a.AccessToken)
	}
}

// TestRefreshMissingExpiresInAdvances —— 核心回归：上游【不返回】expiresIn 时，
// 必须按默认值顺延，而不是保留旧的（已过期的）ExpiresAt。
//
// 保留旧值会让 ExpiresAt 永久卡死在过去：
//   - 面板一直显示那个过期时刻，用户怎么刷新都不变；
//   - NeedsRefresh 恒 true → 每次请求都刷新（刷新风暴）。
func TestRefreshMissingExpiresInAdvances(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 注意：没有 expiresIn
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt"}}`))
	})
	defer srv.Close()

	stale := time.Now().Add(-48 * time.Hour).Unix() // 两天前就过期了
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: stale}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if a.ExpiresAt == stale {
		t.Fatalf("ExpiresAt 仍是旧的过期值 %d（会卡死在面板上）", stale)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt=%d 应顺延到未来", a.ExpiresAt)
	}
	// 应约为 defaultTokenTTL
	want := time.Now().Unix() + int64(defaultTokenTTL/time.Second)
	if a.ExpiresAt < want-5 || a.ExpiresAt > want+5 {
		t.Errorf("ExpiresAt=%d want≈%d (默认 TTL)", a.ExpiresAt, want)
	}
}

// TestRefreshZeroExpiresInAdvances —— expiresIn=0 同样按缺失处理。
func TestRefreshZeroExpiresInAdvances(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":0}}`))
	})
	defer srv.Close()

	stale := time.Now().Add(-1 * time.Hour).Unix()
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: stale}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expiresIn=0 时也应顺延到未来，实际 ExpiresAt=%d", a.ExpiresAt)
	}
}
