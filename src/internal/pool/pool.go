package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/atomicfile"
	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID       string    `json:"uid"`
	Nickname  string    `json:"nickname,omitempty"`
	Credits   int64     `json:"credits"`
	Cooling   bool      `json:"cooling"`
	Until     time.Time `json:"until,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Disabled  bool      `json:"disabled"`
	LowCredit bool      `json:"low_credit"` // 积分低于阈值，仅限 0 费率模型
	ErrCount  int       `json:"err_count,omitempty"`
	// ModelCooling 账号在个别模型上的独立冷却（429/6004 按模型限，账号整体仍可用）。
	// 仅包含仍处于未来的条目；空 = 无模型级冷却。供路由过滤与面板展示。
	ModelCooling   map[string]time.Time `json:"model_cooling,omitempty"`
	LastCheckinOK  bool                 `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time            `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string               `json:"last_checkin_msg,omitempty"`
}

type entry struct {
	a         *auth.Auth
	credits   int64
	disabled  bool
	lowCredit bool // 积分低于阈值，仅限 0 费率模型
	reason    string
	until     time.Time
	errCount  int
	// modelCool 模型级冷却：model → 解冻时刻。
	//
	// 429/6004 上游常按【模型】限额（响应里会提示"您也可以切换其他模型继续使用"），
	// 此时账号整体仍健康 —— 只冷却撞墙的模型，同号其他模型继续参与选号。
	// 若短时间内第二个模型也撞墙，CooldownModel 会升级为整号冷却
	// （防上游其实是按账号计时、逐个模型试探会拖垮账号）。
	// 条目过期后延迟清理（见 CooldownModel）。
	modelCool map[string]time.Time

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// healthyForModel 在 healthy 基础上再过滤模型级冷却。
// model 为空时退化为 healthy（保持旧调用点行为不变）。
func (e *entry) healthyForModel(now time.Time, model string) bool {
	if !e.healthy(now) {
		return false
	}
	if model == "" {
		return true
	}
	t, ok := e.modelCool[model]
	return !ok || !now.Before(t)
}

// stateFile 持久化格式。
type accountState struct {
	Credits        int64                `json:"credits"`
	Disabled       bool                 `json:"disabled"`
	LowCredit      bool                 `json:"low_credit"`
	Reason         string               `json:"reason,omitempty"`
	Until          time.Time            `json:"until,omitempty"`
	ModelCool      map[string]time.Time `json:"model_cooling,omitempty"`
	LastCheckinOK  bool                 `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time            `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string               `json:"last_checkin_msg,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	// lowCredits 低积分阈值：积分低于该值时标记 lowCredit，仅限使用 0 费率模型。
	// <=0 表示关闭该特性。默认 10。
	lowCredits int64

	// reserveCredits 积分保留线：付费请求只从「积分 > 保留线」的账号中选，
	// 余额已落到保留线的账号退出付费轮转，作为底仓留存。
	//
	// 与 lowCredits 的区别（两者独立、可各自开关）：
	//   - lowCredits   ：余额低于阈值 → 冷却到次日等人工/签到恢复，可手工解锁
	//   - reserveCredits：余额 <= 保留线 → 只是不参与**付费**选号，
	//                     0 费率（免费）模型仍可用它
	//
	// 用途：多账号池里给每个账号留一点底仓，避免被单次高消耗请求打空
	// （也顺带避免积分到期作废 —— 前提是能拿到到期日）。
	// <=0 表示关闭（保留全部积分参与轮转，即旧行为）。
	reserveCredits int64
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp, lowCredits: 10}
	if stateFp != "" {
		p.load()
	}
	return p
}

// SetLowCredits 设置低积分阈值；<=0 关闭低积分自动冷却。
func (p *Pool) SetLowCredits(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lowCredits = v
}

// SetReserveCredits 设置积分保留线（<=0 关闭）。
//
// 立即生效：下一次选号即生效，无需重启 —— 底仓语义要求「改完立刻按新线轮转」。
func (p *Pool) SetReserveCredits(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.reserveCredits = v
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		// 凭证被替换（通常是用户重新登录）→ 自动解除禁用。
		//
		// 背景（真实事故）：账号 session 死亡被 Disable 后，用户重新登录覆盖 auth 文件，
		// 但 state.json 里的 disabled 标记仍在，面板一直显示"启用无效"、
		// 引导用户删除重来 —— 其实凭证已经是新的、完全可用。
		//
		// 这里只在 accessToken **真的变了** 时才解除，避免每次目录扫描都无谓重置状态。
		//
		// 解除后若 token 其实仍失效，下一次请求/刷新会失败并重新 Disable —— 不会掩盖问题，
		// 而是让状态自愈到正确值。
		if e.a != nil && a != nil && e.a.AccessToken != a.AccessToken && e.disabled {
			log.Printf("pool: uid=%s 凭证已更新（重新登录），自动解除禁用（原原因：%s）", a.UID, e.reason)
			e.disabled = false
			e.reason = ""
			e.until = time.Time{}
			e.errCount = 0
		}
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的**非低积分**账号；无可用返回 nil。
// 低积分账号（lowCredit）仅限通过 PickLowCredit 选取。
func (p *Pool) Pick() *auth.Auth {
	return p.pickExcludingForModel(true, nil, "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pickExcludingForModel(true, tried, "")
}

// PickLowCredit 返回 healthy 中积分最高的低积分账号（用于 0 费率模型）；无可用返回 nil。
func (p *Pool) PickLowCredit() *auth.Auth {
	return p.pickExcludingForModel(false, nil, "")
}

// PickExcludingLowCredit 同上，但跳过 tried 中的 uid。
func (p *Pool) PickExcludingLowCredit(tried map[string]bool) *auth.Auth {
	return p.pickExcludingForModel(false, tried, "")
}

// PickForModel 选取对【该模型】可用的账号（过滤模型级冷却）。
// model 为空时等价于 Pick()。
func (p *Pool) PickForModel(model string) *auth.Auth {
	return p.PickExcludingForModel(nil, model)
}

// PickExcludingForModel 在排除已试账号的基础上，
// 再排除对该模型处于模型级冷却的账号（429/6004 上游按模型限额时）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, model string) *auth.Auth {
	return p.pickExcludingForModel(true, tried, model)
}

// PickLowCreditForModel 低积分账号池的模型感知版本（用于 0 费率模型）。
func (p *Pool) PickLowCreditForModel(tried map[string]bool, model string) *auth.Auth {
	return p.pickExcludingForModel(false, tried, model)
}

// modelCoolSnapshot 返回仍处于未来的模型级冷却副本（供 Status/持久化使用）。
// 全部过期或为空时返回 nil，避免 JSON 里出现空对象。
func modelCoolSnapshot(in map[string]time.Time) map[string]time.Time {
	if len(in) == 0 {
		return nil
	}
	now := time.Now()
	out := map[string]time.Time{}
	for m, t := range in {
		if now.Before(t) {
			out[m] = t
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CooldownModel 只冷却该账号的【该模型】（429/6004 上游按模型限额时），
// 同号其他模型继续参与选号。返回是否升级为了整号冷却。
//
// 升级规则：当该账号同时有 >= 2 个模型处于模型级冷却时，升级为整号冷却
// （until = 最晚的模型解冻时刻）。
// 依据：若上游其实是按【账号】计时，逐个模型试探会把账号反复拖入冷却，
// 不如一次性按账号冷却到最晚时刻；升级后清 errCount 与 modelCool。
func (p *Pool) CooldownModel(uid, model string, d time.Duration, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	now := time.Now()
	if e.modelCool == nil {
		e.modelCool = map[string]time.Time{}
	}
	e.modelCool[model] = now.Add(d)
	// 统计仍处于未来的模型级冷却，顺便清理过期条目
	active, latest := 0, time.Time{}
	for m, t := range e.modelCool {
		if !now.Before(t) {
			delete(e.modelCool, m)
			continue
		}
		active++
		if t.After(latest) {
			latest = t
		}
	}
	escalated := false
	if active >= 2 {
		e.until = latest
		e.reason = reason + "（多模型接连限流，升级整号冷却）"
		e.errCount = 0
		e.modelCool = nil
		escalated = true
	}
	p.saveLocked()
	return escalated
}

// pickExcludingForModel 选号核心：在 healthy 基础上再按模型过滤。
//   - excludeLowCredit=true  → 排除低积分账号（用于付费模型）
//   - excludeLowCredit=false → 只从低积分账号中选（用于 0 费率模型）
//
// model 为空时 healthyForModel 退化为 healthy，行为与旧版一致。
func (p *Pool) pickExcludingForModel(excludeLowCredit bool, tried map[string]bool, model string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	// fallback 记录「被保留线挡下」的候选：全部账号都落到保留线时，
	// 宁可消耗底仓也不要让用户完全不可用（底仓只是尽量留存，不是硬性隔离）。
	var fallback *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if excludeLowCredit && e.lowCredit {
			continue
		}
		if !excludeLowCredit && !e.lowCredit {
			continue
		}
		// 付费选号：余额已落到保留线的账号退出轮转，留作底仓。
		// 仅作用于付费模型（excludeLowCredit=true）；免费模型仍可用它。
		if excludeLowCredit && p.reserveCredits > 0 && e.credits <= p.reserveCredits {
			if fallback == nil || e.credits > fallback.credits {
				fallback = e
			}
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		best = fallback
	}
	if best == nil {
		return nil
	}
	return best.a
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后更新余额并设置低积分标记：
//   - remain > 0 → 解除冷却（健康）；
//   - remain >= 0 且 remain < 低积分阈值 → 标记 lowCredit（仅限 0 费率模型），
//     否则清除 lowCredit 标记。
//
// remain < 0（查询失败）时保留现有状态。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.credits = remain
	if remain >= 0 && !e.disabled {
		e.until = time.Time{}
		e.reason = ""
		e.errCount = 0
	}
	if p.lowCredits > 0 && remain >= 0 && remain < p.lowCredits && !e.disabled {
		e.lowCredit = true
	} else if remain >= 0 {
		e.lowCredit = false
	}
	p.saveLocked()
}

// Unlock 手工解锁低积分账号：清除 lowCredit 标记与冷却截止，使其恢复为普通账号可用。
func (p *Pool) Unlock(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && !e.disabled {
		e.lowCredit = false
		e.until = time.Time{}
		e.reason = ""
		e.errCount = 0
	}
	p.saveLocked()
}

// SetEnabled 手工禁用/启用账号（面板操作）。启用时同时清除冷却与低积分标记。
func (p *Pool) SetEnabled(uid string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if enabled {
		e.disabled = false
		e.lowCredit = false
		e.until = time.Time{}
		e.reason = ""
		e.errCount = 0
	} else {
		e.disabled = true
		e.reason = "手工禁用"
	}
	p.saveLocked()
	return true
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
	}
	p.saveLocked()
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:            uid,
		Nickname:       e.a.Nickname,
		Credits:        e.credits,
		Cooling:        !e.until.IsZero() && now.Before(e.until),
		Until:          e.until,
		Reason:         e.reason,
		Disabled:       e.disabled,
		LowCredit:      e.lowCredit,
		ErrCount:       e.errCount,
		ModelCooling:   modelCoolSnapshot(e.modelCool),
		LastCheckinOK:  e.lastCheckinOK,
		LastCheckinAt:  e.lastCheckinAt,
		LastCheckinMsg: e.lastCheckinMsg,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:              &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:        s.Credits,
			disabled:       s.Disabled,
			lowCredit:      s.LowCredit,
			reason:         s.Reason,
			until:          s.Until,
			modelCool:      modelCoolSnapshot(s.ModelCool),
			lastCheckinOK:  s.LastCheckinOK,
			lastCheckinAt:  s.LastCheckinAt,
			lastCheckinMsg: s.LastCheckinMsg,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]accountState{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = accountState{
			Credits:        e.credits,
			Disabled:       e.disabled,
			LowCredit:      e.lowCredit,
			Reason:         e.reason,
			Until:          e.until,
			ModelCool:      modelCoolSnapshot(e.modelCool),
			LastCheckinOK:  e.lastCheckinOK,
			LastCheckinAt:  e.lastCheckinAt,
			LastCheckinMsg: e.lastCheckinMsg,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// 与 auth 文件同理：state 也可能被多个进程写（serverd 主进程 + 面板触发的 ctl），
	// 固定 "state.json.tmp" 会有 rename 竞争。用 atomicfile 消除。
	_ = atomicfile.WriteFile(p.stateFp, raw, 0o600)
}
