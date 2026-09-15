package traework

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

func TestDailyCheckinClaimsWhenNotCheckedIn(t *testing.T) {
	var statusCalls, claimCalls atomic.Int32
	var checked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" || r.Header.Get("X-User-Region") != "CN" {
			t.Errorf("missing Trae UG headers: auth=%q region=%q", r.Header.Get("Authorization"), r.Header.Get("X-User-Region"))
		}
		switch r.URL.Path {
		case EpCheckinStatus:
			statusCalls.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"checked_in":%t,"credits":200,"enable":true}`, checked.Load())))
		case EpCheckinClaim:
			claimCalls.Add(1)
			checked.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", DeviceID: "device"}); err != nil {
		t.Fatalf("daily checkin: %v", err)
	}
	if statusCalls.Load() != 2 || claimCalls.Load() != 1 {
		t.Fatalf("status calls=%d claim calls=%d", statusCalls.Load(), claimCalls.Load())
	}
}

func TestDailyCheckinSkipsClaimWhenAlreadyCheckedIn(t *testing.T) {
	var claimCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinStatus {
			_, _ = w.Write([]byte(`{"checked_in":true,"credits":200,"enable":true}`))
			return
		}
		if r.URL.Path == EpCheckinClaim {
			claimCalls.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil || err.Error() != "已签到" {
		t.Fatalf("err=%v, want 已签到", err)
	}
	if claimCalls.Load() != 0 {
		t.Fatalf("claim calls=%d", claimCalls.Load())
	}
}

func TestCheckinClaimBusinessError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err == nil || !strings.Contains(err.Error(), "9074") {
		t.Fatalf("err=%v, want business 9074 error", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("claim calls=%d, want retry", calls.Load())
	}
}

func TestCheckinClaimRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCheckinClaim {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("claim calls=%d", calls.Load())
	}
}

// TestFetchModelsSkipsCustomModels 校验 is_custom_model 与 custom_model_ 前缀
// 两种形态的自定义模型都被过滤掉：这类模型（第三方代理）调用需额外授权，
// 出现在 /v1/models 里会让客户端选中后必然失败。
func TestFetchModelsSkipsCustomModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpModels {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"config_info_list":[
			{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},
			{"config_name":"kimi-k2","display_config":{"display_name":"Kimi K2","is_custom_model":true}},
			{"config_name":"custom_model_proxy-x","display_config":{"display_name":"Proxy X"}},
			{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2 dup"}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.AgentHost = srv.URL
	c.HTTP = srv.Client()
	models, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	if !got["glm-5.2"] {
		t.Errorf("normal model filtered out: %v", got)
	}
	if got["kimi-k2"] {
		t.Errorf("is_custom_model=true not filtered: %v", got)
	}
	if got["custom_model_proxy-x"] {
		t.Errorf("custom_model_ prefix not filtered: %v", got)
	}
	if len(models) != 1 {
		t.Errorf("want 1 model (dedup + filter), got %d: %v", len(models), got)
	}
}
