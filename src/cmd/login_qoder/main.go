// loginqoder.go — QoderWork 登录（url/poll），供 Web 面板子进程调用。
//
//	loginqoder url  -authdir=<dir> -state=<state>
//	               → 生成 Qoder 授权 URL（PKCE 设备流），浏览器打开该 URL 登录
//	loginqoder poll -authdir=<dir> -state=<state>
//	               → 轮询设备令牌，成功后生成 COSY 机器指纹并写 auth 文件，
//	                 打印 JSON 到 stdout
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/login_qoder"
	"github.com/rockswang/workbuddy-wild/internal/qoder"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: loginqoder <url|poll> [-authdir=...] [-state=...]")
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet("loginqoder", flag.ExitOnError)
	authDir := fs.String("authdir", "./auths", "auth storage dir")
	state := fs.String("state", "/tmp/ai-proxy-qoder-login-state.json", "login state json path")
	_ = fs.Parse(os.Args[2:])

	client := login_qoder.NewClient()
	switch sub {
	case "url":
		u, err := login_qoder.Start(client, *state)
		if err != nil {
			fatalf("start: %v", err)
		}
		fmt.Println(u)
	case "poll":
		r, err := login_qoder.Poll(client, *state)
		if err != nil {
			fatalf("poll: %v", err)
		}
		// 生成并保持 COSY 机器指纹，随凭证一起持久化
		au := &auth.Auth{Kind: "qoder", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
		qoder.EnsureFingerprint(au)
		fp, err := login_qoder.SaveAuth(*authDir, r, au.MachineID, au.MachineToken, au.MachineType)
		if err != nil {
			fatalf("save auth: %v", err)
		}
		out := map[string]any{
			"access_token":  r.AccessToken,
			"refresh_token": r.RefreshToken,
			"expires_in":    r.ExpiresIn,
			"uid":           r.UID,
			"nickname":      r.Nickname,
			"file":          fp,
		}
		raw, _ := json.Marshal(out)
		fmt.Println(string(raw))
	default:
		fatalf("unknown subcommand %q (want url|poll)", sub)
	}
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "loginqoder: "+f+"\n", a...)
	os.Exit(1)
}