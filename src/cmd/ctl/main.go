// ctl.go — AI Proxy 管理命令（供 Web 面板子进程调用）。
// 按平台/账号执行 accounts / credits / checkin / refresh，JSON 输出到 stdout。
//
// 用法:
//
//	ctl -mode=accounts  [-p=workbuddy|traework] [-uid=...]
//	ctl -mode=credits   [-p=...] [-uid=...]
//	ctl -mode=checkin   [-p=...] [-uid=...]
//	ctl -mode=refresh   [-p=...] [-uid=...]
//
// -p 缺省为 both（两平台都处理）；-uid 缺省为全部账号。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/svc"
)

var kinds = []provider.Kind{provider.WorkBuddy, provider.TraeWork, provider.Qoder}

func main() {
	log.SetOutput(os.Stderr) // 进度日志走 stderr，不污染 stdout JSON

	cfgPath := flag.String("config", "config.json", "path to config json")
	mode := flag.String("mode", "accounts", "accounts|credits|checkin|refresh")
	p := flag.String("p", "", "platform: workbuddy|traework (empty=both)")
	uid := flag.String("uid", "", "account uid (empty=all)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			if cfg, err = config.Load(""); err != nil {
				fatalf("load config: %v", err)
			}
		} else {
			fatalf("load config: %v", err)
		}
	}
	r, err := svc.New(cfg)
	if err != nil {
		fatalf("svc: %v", err)
	}

	var out any
	switch *mode {
	case "accounts":
		out = collectAccounts(r, wantKind(*p), *uid)
	case "credits":
		out = collectCredits(r, wantKind(*p), *uid)
	case "checkin":
		out = collectCheckin(r, wantKind(*p), *uid)
	case "refresh":
		out = collectRefresh(r, wantKind(*p), *uid)
	default:
		fatalf("unknown mode %q", *mode)
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
}

func wantKind(p string) []provider.Kind {
	if p == "" {
		return kinds
	}
	for _, k := range kinds {
		if k.String() == p {
			return []provider.Kind{k}
		}
	}
	return nil
}

func matchKind(ks []provider.Kind, k provider.Kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// ---------- accounts ----------
type outAccount struct {
	Kind         string `json:"kind"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Credits      int64  `json:"credits"`
	Cooling      bool   `json:"cooling"`
	Disabled     bool   `json:"disabled"`
	Reason       string `json:"reason,omitempty"`
	Until        string `json:"until,omitempty"`
	HasRefresh   bool   `json:"has_refresh"`
	RefreshToken string `json:"-"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain,omitempty"`
}

func collectAccounts(r *svc.Runtime, ks []provider.Kind, uid string) []outAccount {
	var out []outAccount
	for _, k := range kinds {
		if !matchKind(ks, k) {
			continue
		}
		pl := r.Pool(k)
		for _, st := range pl.List() {
			if uid != "" && st.UID != uid {
				continue
			}
			o := outAccount{
				Kind:     k.String(),
				UID:      st.UID,
				Nickname: st.Nickname,
				Credits:  st.Credits,
				Cooling:  st.Cooling,
				Disabled: st.Disabled,
				Reason:   st.Reason,
			}
			if !st.Until.IsZero() {
				o.Until = st.Until.Format("2006-01-02 15:04:05")
			}
			if a := pl.AuthByUID(st.UID); a != nil {
				o.HasRefresh = a.RefreshToken != ""
				o.ExpiresAt = a.ExpiresAt
				o.Domain = a.Domain
			}
			out = append(out, o)
		}
	}
	return out
}

// ---------- credits ----------
type outCredit struct {
	Kind     string `json:"kind"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Credits  int64  `json:"credits"`
	OK       bool   `json:"ok"`
	Msg      string `json:"msg,omitempty"`
}

func collectCredits(r *svc.Runtime, ks []provider.Kind, uid string) []outCredit {
	var out []outCredit
	for _, k := range kinds {
		if !matchKind(ks, k) {
			continue
		}
		up := r.Upstream(k)
		pl := r.Pool(k)
		for _, a := range r.Accounts(k) {
			if uid != "" && a.UID != uid {
				continue
			}
			remain, err := up.UserResource(a)
			o := outCredit{Kind: k.String(), UID: a.UID, Nickname: a.Nickname}
			if err != nil {
				o.Msg = err.Error()
			} else {
				o.Credits = remain
				o.OK = true
				pl.ReenableIfCredits(a.UID, remain)
			}
			out = append(out, o)
		}
	}
	return out
}

// ---------- checkin ----------
type outCheckin = outCredit

func collectCheckin(r *svc.Runtime, ks []provider.Kind, uid string) []outCheckin {
	var out []outCheckin
	for _, k := range kinds {
		if !matchKind(ks, k) {
			continue
		}
		if k == provider.Qoder { // Qoder 无签到活动，跳过
			continue
		}
		sch := r.Scheduler(k)
		pl := r.Pool(k)
		for _, a := range r.Accounts(k) {
			if uid != "" && a.UID != uid {
				continue
			}
			if uid == "" {
				if st, ok := pl.Status(a.UID); ok && st.Disabled {
					continue
				}
			}
			res := checkinOne(sch, a.UID)
			out = append(out, outCheckin{Kind: k.String(), UID: a.UID, Nickname: a.Nickname, Credits: res.Remain, OK: res.OK, Msg: res.Msg})
		}
	}
	return out
}

func checkinOne(sch *scheduler.Scheduler, uid string) scheduler.CheckinResult {
	res, err := sch.CheckinAccount(uid)
	if err != nil {
		return scheduler.CheckinResult{UID: uid, Msg: err.Error()}
	}
	return res
}

// ---------- refresh ----------
func collectRefresh(r *svc.Runtime, ks []provider.Kind, uid string) []outCredit {
	var out []outCredit
	for _, k := range kinds {
		if !matchKind(ks, k) {
			continue
		}
		up := r.Upstream(k)
		for _, a := range r.Accounts(k) {
			if uid != "" && a.UID != uid {
				continue
			}
			o := outCredit{Kind: k.String(), UID: a.UID, Nickname: a.Nickname}
			if a.RefreshToken == "" {
				o.Msg = "no refresh token"
				out = append(out, o)
				continue
			}
			err := up.RefreshToken(a)
			if err != nil {
				o.Msg = err.Error()
			} else if err := a.SaveAtomic(); err != nil {
				o.Msg = "refresh save: " + err.Error()
			} else {
				o.OK = true
				o.Msg = fmt.Sprintf("expires_at=%d", a.ExpiresAt)
			}
			out = append(out, o)
		}
	}
	return out
}

// ---------- 工具 ----------
func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "ctl: "+f+"\n", a...)
	os.Exit(1)
}