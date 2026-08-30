// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
// 注意: 本文件已加入 sync_vendor.sh 的 PROTECT_FILES（addon 独有维护）。
// 在 wild-work 的 clientUA/X-Product 基础上, addon 额外携带 X-IDE-*/X-Product-Version
// 客户端身份头, 使 WorkBuddy 流量统计「客户端」列显示为 workbuddy（与官方桌面端一致）。
package upstream

import (
	"net/http"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

const (
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"

	// addon 客户端身份头（WorkBuddy 官方桌面端对等值）。
	// 官方桌面端 main/server.go 默认 ideType/ideName = "WorkBuddy"；
	// 缺失这些头时 WorkBuddy 流量统计「客户端」列显示为空。
	clientIDEType    = "WorkBuddy"
	clientIDEName    = "WorkBuddy"
	clientIDEVersion = "5.4.4"
	clientProdVer    = "5.4.4"
)

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return originRefererGlobal
	}
	return originRefererCN
}

// CommonHeaders 设置所有 API 共享的请求头。
func CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func ChatHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
	// addon: 客户端身份头 —— WorkBuddy 流量统计「客户端」列依赖它们。
	req.Header.Set("X-IDE-Type", clientIDEType)
	req.Header.Set("X-IDE-Name", clientIDEName)
	req.Header.Set("X-IDE-Version", clientIDEVersion)
	req.Header.Set("X-Product-Version", clientProdVer)
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
