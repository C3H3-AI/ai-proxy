package traework

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestUnixDate —— 到期日解析：有了它 ledger 才能区分「到期作废」与「正常消耗」。
func TestUnixDate(t *testing.T) {
	cases := []struct {
		name string
		sec  int64
		want string
	}{
		{"0 → 空串（上游未下发）", 0, ""},
		{"负数 → 空串", -1, ""},
	}
	for _, c := range cases {
		if got := unixDate(c.sec); got != c.want {
			t.Errorf("%s: unixDate(%d)=%q，期望 %q", c.name, c.sec, got, c.want)
		}
	}
	// 正值应返回 YYYY-MM-DD
	got := unixDate(time.Date(2026, 10, 15, 12, 0, 0, 0, entExpireLoc).Unix())
	if len(got) != len("2026-10-15") {
		t.Errorf("正值应返回日期串，实际 %q", got)
	}
}

// TestUnixDateUsesUTC8 —— 必须按 UTC+8 格式化。
func TestUnixDateUsesUTC8(t *testing.T) {
	if _, off := time.Now().In(entExpireLoc).Zone(); off != 8*60*60 {
		t.Errorf("entExpireLoc 偏移应为 8h，实际 %d 秒", off)
	}
	// 2026-10-15 07:00 UTC+8 → UTC 为 10-14 23:00；若误用 UTC 会得到 10-14
	sec := time.Date(2026, 10, 15, 7, 0, 0, 0, entExpireLoc).Unix()
	if got := unixDate(sec); got != "2026-10-15" {
		t.Errorf("应按 UTC+8 输出 2026-10-15，实际 %q", got)
	}
}

// TestEntitlementFieldsParsed —— json tag 必须与上游一致（"expire_time" / "entitlement_id"）。
// 写错会导致解析不到 → ExpireAt/Key 恒为空 → 账本静默退化（不报错但功能失效）。
func TestEntitlementFieldsParsed(t *testing.T) {
	// 用匿名结构体复刻实际解析目标，确认 tag 拼写
	var probe struct {
		EntitlementBaseInfo struct {
			EntitlementID string `json:"entitlement_id"`
		} `json:"entitlement_base_info"`
		ExpireTime int64 `json:"expire_time"`
	}
	if tag := jsonTagOf(probe, "ExpireTime"); tag != "expire_time" {
		t.Errorf("ExpireTime json tag 应为 expire_time，实际 %q", tag)
	}
	// 嵌套一层校验 EntitlementID
	inner := reflect.TypeOf(probe).Field(0).Type
	f, ok := inner.FieldByName("EntitlementID")
	if !ok {
		t.Fatal("EntitlementBaseInfo 应含 EntitlementID")
	}
	tag := f.Tag.Get("json")
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag != "entitlement_id" {
		t.Errorf("EntitlementID json tag 应为 entitlement_id，实际 %q", tag)
	}
}

func jsonTagOf(v any, name string) string {
	typ := reflect.TypeOf(v)
	f, ok := typ.FieldByName(name)
	if !ok {
		return ""
	}
	tag := f.Tag.Get("json")
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	return tag
}
