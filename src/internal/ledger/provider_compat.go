// provider_compat.go 本地 fork 适配层：ledger 依赖的 provider 符号补齐。
//
// 背景：上游 wild-work@v2.6.2 的 provider.ResourceItem 有 8 个字段，且 provider 包
// 还提供 ResourceItem 语义配套的 Summarize / ExpiringWithin。本地 fork 的
// provider.ResourceItem（internal/provider/provider.go，CODE GENERATED @c62d0bc）
// 只有 Name/Total/Used/Remain 四个字段——ExpireAt / Key / Usable / InfoOnly 四个
// 字段与 ExpiringWithin 都不存在。
//
// 规则（任务约束）：**不修改 provider 包**（它是 sync_vendor.sh 生成物，改了下次
// 同步会被覆盖），故在 ledger 包内做最小本地实现：
//
//  1. ResourceItem —— provider.ResourceItem 的超集，字段与上游 v2.6.2 逐一对齐
//     （含 json tag），保证 ledger 的差分语义不被阉割：
//       - Key      差分对账键（缺了就退回 Name 伪键，月周期切换时会误判）
//       - ExpireAt 到期日（缺了就无法区分 spend / expire）
//       - Usable   可消耗池标记（缺了会把不可用池的变动算进 earn/spend）
//     其中 ExpireAt 与 Usable 直接关系 R43：ledger 之所以与 stats 刻意不合并，
//     正是因为差值口径含「积分包到期作废」——没有 ExpireAt 就没有 expire 口径，
//     双流水账本的设计前提就没了。
//  2. ExpiringWithin —— 临期额度统计，语义与上游 provider 版完全一致（见下方注释）。
//  3. FromProviderItems —— provider.ResourceItem → 本地 ResourceItem 的转换，
//     供 HTTP 接线直接吃 UserResourceDetail 的返回值。
//
// 本文件不引入任何新的业务规则，只是把上游 provider 侧的能力在 ledger 内复刻一份。
package ledger

import (
	"time"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// ResourceItem 积分明细条目：上游 provider.ResourceItem 的完整字段集。
//
// 上游注释（v2.6.2，internal/provider/provider.go）原文摘录：
//   - ExpireAt 该条目到期时刻（空串表示上游未下发到期时间，前端据此隐藏「有效期」
//     列——不得用零值时间冒充「永不过期」）。
//   - Key 条目稳定标识（上游提供的 ID，如 TraeWork entitlement_id），供 ledger
//     差分对账用；渠道无 ID 时留空，差分退回 Name 作伪键。
//   - Usable 标记该条目是否属于本工具可消耗的额度池。TraeWork 存在按
//     available_endpoint 划分的专用池（ep=1，官方客户端专用），本工具走 ep=0；
//     这类额度对用户是「看得见用不了」，需在界面上分开统计。
//     注意：零值为 false，故各渠道构造时须显式置位；渠道无此概念时统一填 true。
//   - InfoOnly 标记该条目只作展示，不参与任何积分算术（Summarize 小计、
//     ExpiringWithin 临期、ledger 差分）。用于「同一账号下计量单位不同的另一套
//     额度」——如 MonkeyCode 的每日 Token 额度（单位是 token，而积分是 credits）：
//     两者都该显示，但相加无意义。
//     注意：InfoOnly 条目应同时置 Usable=true。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`

	ExpireAt string `json:"expire_at,omitempty"`
	Key      string `json:"key,omitempty"`
	Usable   bool   `json:"usable"`
	InfoOnly bool   `json:"info_only,omitempty"`
}

// ExpiringWithin 统计 horizon 时长内到期的**可消耗**积分小计（临期额度）。
//
// 上游 provider.ExpiringWithin 的等价实现（本地 provider 包无此函数，按任务要求
// 在 ledger 包内复刻，语义逐条对齐上游注释）：
//   - 仅累计 Usable=true 且 ExpireAt 非空的条目——不可用池本工具消耗不到，
//     临期与否不影响路由决策，混入会虚高临期值；
//   - InfoOnly 条目不计（它不参与任何积分算术）；
//   - ExpireAt 是 YYYY-MM-DD 日期粒度（UTC+8 墙钟），精确到小时的 24h 判定无意义，
//     实际口径为「到期日 ≤ 明天」：今天到期/明天到期都算临期，后天起不算；
//   - ExpireAt 为空串（上游未下发，如 Qoder）或不可解析时不计入，调用方无需特判。
//
// 注：ledger 自身的写入/差分链路并不调用它（上游 credit.go 只在注释里对齐口径，
// 用的是同 expireLoc 的 expired()）；放在这里是为了 HTTP 接口展示「临期额度」时
// 有与上游一致的口径，且免去调用方各写一份循环。
func ExpiringWithin(items []ResourceItem, horizon time.Duration) int64 {
	deadline := time.Now().In(expireLoc).Add(horizon)
	var expiring int64
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		// 日期解析到当天零点（UTC+8），零点落在 deadline 之前即视为临期
		if t, err := time.ParseInLocation("2006-01-02", it.ExpireAt, expireLoc); err == nil && t.Before(deadline) {
			expiring += it.Remain
		}
	}
	return expiring
}

// Summarize 按 Usable 标记汇总条目：返回 (可消耗剩余, 不可消耗剩余)。
// 上游 provider.Summarize 的等价实现（同因本地 provider 缺失而复刻）。
func Summarize(items []ResourceItem) (usable, unusable int64) {
	for _, it := range items {
		if it.InfoOnly {
			continue
		}
		if it.Usable {
			usable += it.Remain
		} else {
			unusable += it.Remain
		}
	}
	return usable, unusable
}

// FromProviderItems 把本地 provider.UserResourceDetail 返回的条目转成 ledger 口径。
//
// 本地三个渠道（upstream/traework/qoder）构造 provider.ResourceItem 时只填
// Name/Total/Used/Remain，四个扩展字段按上游注释的默认值补齐：
//   - Usable=true   —— 本地 provider 无「可用池/不可用池」划分（那是 TraeWork
//     available_endpoint 的概念，本地 traework 客户端未下发该标记），
//     按上游「渠道无此概念时统一填 true」处理；
//   - ExpireAt=""   —— 本地未下发到期日，按上游「空串 = 上游未下发」处理；
//     后果：expired() 恒 false，余额下降一律归因 spend。即本地当前口径下
//     expire 只由「条目整体消失」触发（DiffCredits 末尾那段），不会由到期日触发。
//   - Key=""        —— 差分退回 Name 伪键（itemKey 的行为）；
//   - InfoOnly=false。
//
// 后续若渠道补齐了到期日/稳定 ID，应在**渠道构造侧**填好后改走 ResourceItem
// 入参的 DiffCredits，而不是在这里猜。
func FromProviderItems(items []provider.ResourceItem) []ResourceItem {
	if items == nil {
		return nil
	}
	out := make([]ResourceItem, 0, len(items))
	for _, it := range items {
		out = append(out, ResourceItem{
			Name:   it.Name,
			Total:  it.Total,
			Used:   it.Used,
			Remain: it.Remain,
			Usable: true,
		})
	}
	return out
}
