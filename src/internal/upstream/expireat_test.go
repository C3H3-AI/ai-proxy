package upstream

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestResourceAccountExpireAt —— 到期日解析：
// 有了 ExpireAt，ledger 才能把余额下降归因到「周期结束作废」而非「正常消耗」。
func TestResourceAccountExpireAt(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"正常墙钟串", "2026-10-15 23:59:59", "2026-10-15"},
		{"当天", "2026-10-10 00:00:00", "2026-10-10"},
		{"空串（上游未下发）", "", ""},
		{"仅空白", "   ", ""},
		{"格式非法", "not-a-time", ""},
		{"日期粒度（无时分秒）", "2026-10-15", ""}, // 严格按上游布局解析
	}
	for _, c := range cases {
		r := resourceAccount{CycleEndTime: c.in}
		if got := r.expireAt(); got != c.want {
			t.Errorf("%s: expireAt()=%q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestExpireAtUsesUTC8 —— 时区必须是 UTC+8，否则跨天条目会归到错误的日期。
func TestExpireAtUsesUTC8(t *testing.T) {
	// 2026-10-15 07:00:00（UTC+8）→ UTC 是 2026-10-14 23:00；
	// 若误用 UTC 解析会得到 10-14，正是要防止的错误。
	r := resourceAccount{CycleEndTime: "2026-10-15 07:00:00"}
	if got := r.expireAt(); got != "2026-10-15" {
		t.Errorf("应按 UTC+8 解析为 2026-10-15，实际 %q", got)
	}
	// 对照：resourceExpireLoc 确实是 +8
	if _, off := time.Now().In(resourceExpireLoc).Zone(); off != 8*60*60 {
		t.Errorf("resourceExpireLoc 偏移应为 8h，实际 %d 秒", off)
	}
}

// TestResourceAccountParsesCycleEndTime —— JSON 字段必须能解析出来（否则 ExpireAt 恒为空，功能静默失效）。
func TestResourceAccountParsesCycleEndTime(t *testing.T) {
	// 注意：字段名与大小写必须与上游一致（"CycleEndTime"）
	var r resourceAccount
	if r.CycleEndTime != "" {
		t.Fatal("初始值应为空")
	}
	// 通过反射确认 tag 正确（防止手滑写错 json tag 导致解析不到又不报错）
	f, ok := reflectTypeField(r, "CycleEndTime")
	if !ok {
		t.Fatal("resourceAccount 应含 CycleEndTime 字段")
	}
	if f != "CycleEndTime" {
		t.Errorf("json tag 应为 CycleEndTime，实际 %q", f)
	}
}

// reflectTypeField 返回该字段的 json tag（去掉选项），不存在返回 ("", false)。
// 用于防止「json tag 写错 → 解析不到 → ExpireAt 恒为空且不报错」这类静默失效。
func reflectTypeField(v any, name string) (string, bool) {
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	f, ok := typ.FieldByName(name)
	if !ok {
		return "", false
	}
	tag := f.Tag.Get("json")
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	return tag, true
}
