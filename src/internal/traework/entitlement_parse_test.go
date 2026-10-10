package traework

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// 真实上游响应结构(抓包取得,字段位置与线上一致,不要凭印象改。
//
// 关键:entitlement_id 在 entitlement_base_info 内层**,不是外层。
// 早先把它放在外层 → 解析恒为空,差分退回 Name 伪键;而 Name 伪键在「同一套餐名的相邻两天」(如两条都叫"签到奖励",而真实标识是 checkin_20261010_<uid> —— 靠日期区分。
// 用伪键会把两天撞成一键,周期切换被误判成条目变动。

const realEntResp = `{
  "is_credits_billing": true,
  "usage_summary": {"consumed_amount": 4587.9, "total_amount": 4750},
  "user_entitlement_pack_list": [
    {
      "display_desc": "签到奖励",
      "expire_time": 1794240004,
      "entitlement_base_info": {
        "entitlement_id": "checkin_20261010_3810278351841783",
        "package_name": "签到奖励",
        "end_time": 1794240004,
        "available_endpoint": 0,
        "quota": {"credits_limit": 100},
        "usage": {"credits_amount": 0},
        "group_type": 0
      }
    },
    {
      "display_desc": "免费",
      "expire_time": 1793462399,
      "entitlement_base_info": {
        "entitlement_id": "free_utc202610_3810278351841783",
        "package_name": "免费",
        "end_time": 1793462399,
        "available_endpoint": 0,
        "quota": {"credits_limit": 50},
        "usage": {"credits_amount": 10}
      }
    }
  ]
}`

// TestEntitlementParseRealResponse —— 端到端解析校验(用真实 JSON)。
//
// 这个用例存在的理由:之前的测试只检查「本地 probe 结构体的 json tag」,
// 抓不到「字段放错嵌套层级」这类 bug —— 而真实线上数据就是栽在这里。

func TestEntitlementParseRealResponse(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realEntResp))
	}))
	defer srv.Close()

	c := New()
	c.UgHost = srv.URL
	c.HTTP = srv.Client()

	total, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if gotPath != EpEntUsage {
		t.Errorf("请求路径=%s,期望 %s", gotPath, EpEntUsage)
	}
	if len(items) != 2 {
		t.Fatalf("条目数=%d,期望 2", len(items))
	}
	if total <= 0 {
		t.Errorf("total=%d,应 > 0", total)
	}

	// 关键:entitlement_id 必须解析出来(不是空)
	if items[0].Key == "" {
		t.Fatal(`Key 为空 —— entitlement_id 没解析到(检查它是否在 entitlement_base_info 内层`)
	}
	if want := "checkin_20261010_3810278351841783"; items[0].Key != want {
		t.Errorf("Key=%q,期望 %q", items[0].Key, want)
	}
	// 两条"签到奖励"若不靠 entitlement_id 区分就会撞键;这里两条 Key 必须不同
	if items[0].Key == items[1].Key {
		t.Errorf("两条不同条目 Key 相同(%q) —— 会误判成同条目", items[0].Key)
	}
	// 到期日必须解析出来(供账本区分「过期」与「消耗」)
	if items[0].ExpireAt == "" {
		t.Fatal("ExpireAt 为空 —— 到期日没解析到")
	}
	// 1794240004 -> 2026-11-10(UTC+8)
	if want := "2026-11-10"; items[0].ExpireAt != want {
		t.Errorf("ExpireAt=%q,期望 %q", items[0].ExpireAt, want)
	}
	// 必须标记为可消耗,否则账本会忽略它(Usable 零值是 false)
	if !items[0].Usable {
		t.Error("Usable 应为 true(零值 false 会让账本忽略条目)")
	}
	_ = time.Now
}
