// logintrae.go — TraeWork 登录（url/complete/poll），供 Web 面板子进程调用。
//
//	logintrae url       -state=<state> -callback=<panel_cb_url>
//	                     → 生成 Trae 授权 URL（登录完成后授权页重定向到 callback）
//	logintrae complete  -state=<state> -url=<回调完整URL>
//	                     → 解析回调、换 token、拿账号信息，打印 JSON 到 stdout
//	logintrae poll      -state=<state>
//	                     → 轮询检查回调是否已到达，完成后换 token 并输出 JSON
//
// 回调指向面板自身的公共端点（如 http://<ha>:7863/api/trae-cb），
// 浏览器登录后直接跳回面板，无需额外回调端口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/login_trae"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: logintrae <url|complete|poll> [-state=...] [-callback=...] [-url=...]")
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet("logintrae", flag.ExitOnError)
	state := fs.String("state", "/tmp/ai-proxy-trae-login-state.json", "login state json path")
	cb := fs.String("callback", "", "panel callback url, e.g. http://<ha>:7863/api/trae-cb")
	rawURL := fs.String("url", "", "raw callback url captured from browser redirect")
	refresh := fs.String("refresh", "", "raw refreshToken for direct exchange (no browser callback needed)")
	host := fs.String("host", "", "oauth host for -refresh, default api.trae.com.cn")
	_ = fs.Parse(os.Args[2:])

	switch sub {
	case "url":
		if *cb == "" {
			fatalf("-callback is required")
		}
		u, err := login_trae.AuthURL(*state, *cb)
		if err != nil {
			fatalf("start: %v", err)
		}
		fmt.Println(u)
	case "complete":
		client := &http.Client{Timeout: 30 * time.Second}
		var res login_trae.Result
		var err error
		if *refresh != "" {
			// 直接 refreshToken 换 token（最稳，不依赖回调）
			res, err = login_trae.CompleteRefresh(client, *refresh, *host)
		} else {
			if *rawURL == "" {
				fatalf("-url is required (or use -refresh)")
			}
			res, err = login_trae.Complete(client, *state, *rawURL)
		}
		if err != nil {
			fatalf("complete: %v", err)
		}
		printResult(res)
	case "poll":
		client := &http.Client{Timeout: 30 * time.Second}
		res, err := login_trae.Poll(client, *state)
		if err != nil {
			fatalf("poll: %v", err)
		}
		printResult(res)
	default:
		fatalf("unknown subcommand %q (want url|complete|poll)", sub)
	}
}

func printResult(res login_trae.Result) {
	out := map[string]any{
		"access_token":  res.AccessToken,
		"refresh_token": res.RefreshToken,
		"expires_at":    res.ExpiresAt,
		"domain":        res.Domain,
		"api_host":      res.ApiHost,
		"machine_id":    res.MachineID,
		"device_id":     res.DeviceID,
		"uid":           res.UID,
		"enterprise_id": res.EnterpriseID,
		"nickname":      res.Nickname,
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "logintrae: "+f+"\n", a...)
	os.Exit(1)
}