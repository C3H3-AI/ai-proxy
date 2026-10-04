package qoder

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCosyDateMatchesSignatureTimestamp —— 回归：cosy-date 必须与签名同源。
//
// 上游拿 cosy-date 头重算签名。若 AuthHeader 里算签名用一次 time.Now()、
// ApplyHeaders 设头时又取一次，两者一旦跨秒就对不上，回
//   {"code":"101","message":"Signature invalid"}
//
// 表现：长会话偶发、body 越大越频繁（签名串含整个 body，md5 耗时随 body 增长），
// 且原样重试常能成功 —— 典型的时间竞态，此前极难定位。
//
// 本测试注入一个「每次取值前进 1 秒」的时钟：
// 如果代码里还有第二处 time.Now()，cosy-date 就会与签名所用的时间不同。
func TestCosyDateMatchesSignatureTimestamp(t *testing.T) {
	orig := nowUnix
	defer func() { nowUnix = orig }()

	var calls int
	nowUnix = func() int64 {
		calls++
		return 1700000000 + int64(calls) // 每次调用都前进 1 秒
	}

	s := &CosySession{
		Info:        "info",
		CosyKey:     "key",
		MachineType: "mt",
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/algo/chat", nil)
	if err := s.ApplyHeaders(req, `{"a":1}`, "https://example.com/algo/chat", "u1", true, "m"); err != nil {
		t.Fatalf("ApplyHeaders: %v", err)
	}

	got := req.Header.Get("cosy-date")
	// 第一次调用是签名所用的时间戳（1700000001）；
	// 若存在第二处取时，这里会变成 1700000002 或更大。
	if got != "1700000001" {
		t.Fatalf("cosy-date=%s，应为签名所用的 1700000001（说明还有一处独立取时）", got)
	}
	if calls != 1 {
		t.Errorf("取时次数=%d want 1（全链路只应取一次时间）", calls)
	}
}

// TestCosyDateStableAcrossLargeBody —— body 越大越容易暴露：
// 即便 md5 耗时让两次取时跨秒，cosy-date 也必须等于签名所用的时间戳。
func TestCosyDateStableAcrossLargeBody(t *testing.T) {
	orig := nowUnix
	defer func() { nowUnix = orig }()

	var calls int
	nowUnix = func() int64 {
		calls++
		return 1700000000 + int64(calls)*5 // 每次前进 5 秒，放大差异
	}

	s := &CosySession{Info: "info", CosyKey: "key", MachineType: "mt"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/algo/chat", nil)
	big := make([]byte, 0, 1<<20)
	for i := 0; i < 1<<20; i++ {
		big = append(big, 'x')
	}
	if err := s.ApplyHeaders(req, string(big), "https://example.com/algo/chat", "u1", true, "m"); err != nil {
		t.Fatalf("ApplyHeaders: %v", err)
	}
	if got := req.Header.Get("cosy-date"); got != "1700000005" {
		t.Fatalf("cosy-date=%s want 1700000005（签名所用的时间戳）", got)
	}
	if calls != 1 {
		t.Errorf("取时次数=%d want 1", calls)
	}
}

// TestCosyAuthHeaderReturnsDate —— AuthHeader 必须返回签名所用的时间戳。
func TestCosyAuthHeaderReturnsDate(t *testing.T) {
	orig := nowUnix
	defer func() { nowUnix = orig }()
	nowUnix = func() int64 { return 1700000042 }

	s := &CosySession{Info: "info", CosyKey: "key", MachineType: "mt"}
	auth, date, err := s.AuthHeader(`{"a":1}`, "https://example.com/algo/chat", "u1")
	if err != nil {
		t.Fatalf("AuthHeader: %v", err)
	}
	if auth == "" {
		t.Error("auth 不应为空")
	}
	if date != 1700000042 {
		t.Errorf("date=%d want 1700000042", date)
	}
}
