package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// TestHumanizeRefreshErrorSessionDead —— 核心：session 死亡必须给出
// "重新登录" 的可操作提示，而不是原始英文 JSON 长串。
func TestHumanizeRefreshErrorSessionDead(t *testing.T) {
	err := &provider.Error{
		Kind:   provider.ErrSessionDead,
		Status: 401,
		Msg:    `{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: Token is not active"}`,
	}
	got := humanizeRefreshError(err)
	if !strings.Contains(got, "重新登录") {
		t.Errorf("应提示重新登录，实际: %s", got)
	}
	if strings.Contains(got, "invalid_grant") {
		t.Errorf("不应把原始报文直接抛给用户，实际: %s", got)
	}
}

// TestHumanizeRefreshErrorKinds —— 各分类都要有中文提示。
func TestHumanizeRefreshErrorKinds(t *testing.T) {
	cases := []struct {
		kind provider.ErrKind
		want string
	}{
		{provider.ErrSessionDead, "重新登录"},
		{provider.ErrHardCredit, "余额"},
		{provider.ErrSoftRate, "限流"},
		{provider.ErrPassthrough, "排队"},
		{provider.ErrAccountFault, "授权"},
		{provider.ErrServer, "故障"},
	}
	for _, c := range cases {
		err := &provider.Error{Kind: c.kind, Status: 500, Msg: "x"}
		got := humanizeRefreshError(err)
		if !strings.Contains(got, c.want) {
			t.Errorf("kind=%v 提示应含 %q，实际: %s", c.kind, c.want, got)
		}
	}
}

// TestHumanizeRefreshErrorUnknownKeepsRaw —— 无法识别时必须保留原文，
// 不吞掉排查所需信息。
func TestHumanizeRefreshErrorUnknownKeepsRaw(t *testing.T) {
	raw := errors.New("some weird failure: xyz")
	got := humanizeRefreshError(raw)
	if !strings.Contains(got, "some weird failure: xyz") {
		t.Errorf("应保留原始错误，实际: %s", got)
	}
	if !strings.HasPrefix(got, "刷新失败") {
		t.Errorf("应有中文前缀，实际: %s", got)
	}
}

func TestHumanizeRefreshErrorNil(t *testing.T) {
	if got := humanizeRefreshError(nil); got != "" {
		t.Errorf("nil 应返回空串，实际: %q", got)
	}
}

// TestCollectRefreshUsesHumanizedMessage —— 验证 humanizeRefreshError
// 【真的被接入】了 collectRefresh，而不是只定义了函数却没用。
//
// 做法：读 main.go 源码，确认 collectRefresh 里不再有 o.Msg = err.Error()。
// （直接测 collectRefresh 需要构造完整 svc.Runtime，代价大；
//   而这个"接线检查"能抓住"定义了函数但忘记调用"的常见疏漏。）
func TestCollectRefreshUsesHumanizedMessage(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)

	// 只取 collectRefresh 函数体，避免误伤 collectCredits 等同名的 o.Msg 赋值
	start := strings.Index(src, "func collectRefresh(")
	if start < 0 {
		t.Fatal("未找到 collectRefresh")
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	body := rest
	if end >= 0 {
		body = rest[:end]
	}

	if strings.Contains(body, "o.Msg = err.Error()") {
		t.Error("collectRefresh 仍直接输出原始错误，未接入 humanizeRefreshError")
	}
	if !strings.Contains(body, "o.Msg = humanizeRefreshError(err)") {
		t.Error("collectRefresh 未调用 humanizeRefreshError")
	}
}
