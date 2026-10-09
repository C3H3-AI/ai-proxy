// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rockswang/workbuddy-wild/internal/ledger"
	"github.com/rockswang/workbuddy-wild/internal/stats"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// Runtime 是一个平台的一组运行时资源：pool + upstream + 静态模型兜底。
type Runtime struct {
	Kind         provider.Kind
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo

	mu       sync.RWMutex
	models   []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// Config handler 依赖。
type Config struct {
	Runtimes map[provider.Kind]*Runtime
	APIKey   string // 空 = 不鉴权

	// Stats 运行统计引擎 / Ledger 双流水账本（由 svc.Runtime 初始化后传入）。
	// 两者是**旁路能力**，可能为 nil（初始化失败时降级），
	// 消费处必须先判空（见 h.stats / h.ledger）。
	Stats  *stats.Stats
	Ledger *ledger.Ledger

	// 兼容旧调用方：只传 Pool/Upstream 时等价于只启用 workbuddy。
	Pool     *pool.Pool
	Upstream provider.Upstream

	MaxRotate    int
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration

	// PricingFunc 返回指定渠道的模型定价列表（用于 cheapest 模型选择费率最低的模型）。
	// 为 nil 时 cheapest 退化为第一个可用模型。
	PricingFunc func(kind provider.Kind) []provider.ModelPricing
	// AutoModels 每个渠道的 cheapest 候选模型白名单（空 = 使用定价列表全部）。
	AutoModels map[provider.Kind][]string
}

// defaultRefreshSkew 返回按平台的默认 token 预刷新窗口。
//
// TraeWork 的 access token 生命周期短且上游可能提前吊销，10 分钟窗口过窄：
// 一旦请求途中 token 失效，上游返回 401 会被 Classify 判为 ErrSessionDead，
// 进而 Pool.Disable 永久禁用账号（需人工重登）。traework2api 用 24h 窗口正是
// 为此，这里与其对齐。WorkBuddy 侧 10 分钟已足够，保持不变。
func defaultRefreshSkew(kind provider.Kind) time.Duration {
	if kind == provider.TraeWork {
		return 24 * time.Hour
	}
	return 10 * time.Minute
}

// refreshSkewFor 解析指定平台实际使用的预刷新窗口：
// cfg.RefreshSkew 显式配置时仍以全局值为准（保持旧行为、便于统一调参），
// 未配置（<=0）或低于平台默认值时取平台默认值——窗口只放宽不收紧，
// 避免把 TraeWork 又调回过窄区间。
func (h *Handler) refreshSkewFor(kind provider.Kind) time.Duration {
	base := defaultRefreshSkew(kind)
	if h.cfg.RefreshSkew > base {
		return h.cfg.RefreshSkew
	}
	return base
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	apiMu    sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // runtimeKind → stickyEntry（粘性路由，提升会话缓存利用率）
}

func NewHandler(cfg Config) *Handler {
	if cfg.Runtimes == nil && cfg.Pool != nil && cfg.Upstream != nil {
		cfg.Runtimes = map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: cfg.Pool, Upstream: cfg.Upstream, StaticModels: WorkBuddyStaticModels()},
		}
	}
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = defaultRefreshSkew(provider.WorkBuddy)
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry)}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("POST /api/accounts/unlock", h.withAuth(h.unlockAccount))
	h.mux.HandleFunc("POST /api/accounts/enable", h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		h.setAccountEnabled(w, r, true)
	}))
	h.mux.HandleFunc("POST /api/accounts/disable", h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		h.setAccountEnabled(w, r, false)
	}))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /api/stats", h.withAuth(h.statsSnapshot))
	h.mux.HandleFunc("GET /api/ledger", h.withAuth(h.ledgerQuery))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// stickyEntry 粘性路由记录：记录上次路由账号及连续使用次数。
// 不使用 credits（pool 中余额仅在签到/手动刷新时更新，对话后是 stale 数据），
// 改用请求计数：连续请求 maxReqs 次后自动降级换账号。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
}

// stickyKey 粘性路由 key（按渠道独立）
func (h *Handler) stickyKey(kind provider.Kind) string { return kind.String() }

// pickWithSticky 粘性路由选择账号。
// freeModel=false 表示付费模型：仅高积分（非低积分）账号可用。
// freeModel=true  表示 0 费率模型：高积分与低积分账号均可用，优先选取低积分账号，
//
//	无低积分账号时回退到高积分账号。
//
// model 为本次请求的模型键；用于过滤【模型级冷却】（429/6004 上游按模型限额时，
// 该账号可能只对某些模型冷却、对其他模型仍可用 —— 见 Pool.CooldownModel）。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用/未被当前模型模式允许/对该模型处于模型级冷却
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级选新账号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime, freeModel bool, model string) *auth.Auth {
	const defaultMaxReqs = 50

	// ⚠️ 必须在锁内把字段**拷贝出来**，不能只取指针。
	// stickyEntry 是共享对象：stickySuccess 在写锁内改 reqCount，
	// 若这里解锁后再读 sticky.reqCount，就与写者构成 DATA RACE
	// （-race 下可复现：pickWithSticky:154 读 vs stickySuccess:195 写）。
	h.stickyMu.RLock()
	e := h.sticky[h.stickyKey(rt.Kind)]
	var (
		uid      string
		reqCount int
		maxReqs  int
	)
	if e != nil {
		uid, reqCount, maxReqs = e.uid, e.reqCount, e.maxReqs
	}
	h.stickyMu.RUnlock()

	// 尝试粘性路由：账号必须健康，且对当前模型模式可用（付费模型要求非低积分），
	// 且对该模型不处于模型级冷却
	if uid != "" && reqCount < maxReqs {
		acct := rt.Pool.AuthByUID(uid)
		if acct != nil {
			status, ok := rt.Pool.Status(uid)
			modelCooled := ok && model != "" && status.ModelCooling != nil &&
				status.ModelCooling[model].After(time.Now())
			if ok && !status.Cooling && !status.Disabled && !modelCooled && (freeModel || !status.LowCredit) {
				log.Printf("sticky route platform=%s uid=%s count=%d/%d free_model=%v",
					rt.Kind, uid, reqCount, maxReqs, freeModel)
				return acct
			}
		}
	}

	// 降级：选择账号（排除对该模型处于模型级冷却的账号）
	var acct *auth.Auth
	if freeModel {
		acct = rt.Pool.PickLowCreditForModel(nil, model) // 0 费率模型优先消耗低积分账号
		if acct == nil {
			acct = rt.Pool.PickForModel(model) // 无低积分账号时退回高积分账号
		}
	} else {
		acct = rt.Pool.PickForModel(model)
	}
	if acct == nil {
		return nil
	}

	// 新建粘性记录
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d free_model=%v", rt.Kind, acct.UID, defaultMaxReqs, freeModel)
	return acct
}

// stickySuccess 粘性路由成功：递增请求计数。
func (h *Handler) stickySuccess(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	key := h.stickyKey(rt.Kind)
	if e := h.sticky[key]; e != nil {
		e.reqCount++
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除粘性记录，下次请求强制重新选号。
func (h *Handler) stickyClear(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind))
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := h.currentAPIKey(); key != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != key {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) SetAPIKey(key string) { h.apiMu.Lock(); defer h.apiMu.Unlock(); h.cfg.APIKey = key }
func (h *Handler) currentAPIKey() string {
	h.apiMu.RLock()
	defer h.apiMu.RUnlock()
	return h.cfg.APIKey
}
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	accounts := map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		accounts[k.String()] = rt.Pool.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// accountReq 账号类管理接口的请求体。
type accountReq struct {
	Kind string `json:"kind"`
	UID  string `json:"uid"`
}

// accountTarget 解析请求并定位账号；参数非法/账号不存在时已写出错误响应，返回 ok=false。
func (h *Handler) accountTarget(w http.ResponseWriter, r *http.Request) (*Runtime, string, bool) {
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return nil, "", false
	}
	if req.Kind == "" || req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind and uid are required"})
		return nil, "", false
	}
	rt := h.cfg.Runtimes[provider.Kind(req.Kind)]
	if rt == nil || rt.Pool == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not configured: " + req.Kind})
		return nil, "", false
	}
	if _, ok := rt.Pool.Status(req.UID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found: " + req.UID})
		return nil, "", false
	}
	return rt, req.UID, true
}

// unlockAccount 手工解锁低积分账号。body: {"kind":"workbuddy","uid":"..."}
func (h *Handler) unlockAccount(w http.ResponseWriter, r *http.Request) {
	rt, uid, ok := h.accountTarget(w, r)
	if !ok {
		return
	}
	st, _ := rt.Pool.Status(uid)
	if st.Disabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account is permanently disabled, cannot unlock"})
		return
	}
	rt.Pool.Unlock(uid)
	st, _ = rt.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

// setAccountEnabled 面板手工启用/禁用账号。body: {"kind":"workbuddy","uid":"..."}
// 必须由 serverd 常驻进程执行：账号禁用标记同时存在于内存池与 state.json，
// ctl 子进程只改文件、serverd 不知道，其下一次落盘会把文件覆盖回旧值，
// 表现为「解禁了又禁用」。这里直接改内存池并由其落盘，保证两者一致。
func (h *Handler) setAccountEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	rt, uid, ok := h.accountTarget(w, r)
	if !ok {
		return
	}
	if !rt.Pool.SetEnabled(uid, enabled) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found: " + uid})
		return
	}
	st, _ := rt.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

var workbuddyStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2", ContextWindow: 131072}, {ID: "glm-5.1", ContextWindow: 131072}, {ID: "glm-5v-turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", ContextWindow: 131072}, {ID: "minimax-m3", ContextWindow: 131072}, {ID: "hy3", ContextWindow: 131072},
	{ID: "hy3-preview", ContextWindow: 131072}, {ID: "hy3-preview-agent", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", ContextWindow: 131072}, {ID: "deepseek-v4-flash", ContextWindow: 131072},
}

var traeworkStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2"}, {ID: "glm-5-turbo"}, {ID: "glm-5"}, {ID: "DeepSeek-V4-Pro"}, {ID: "DeepSeek-V4-Flash"},
	{ID: "kimi-k2.6"}, {ID: "kimi-k2.7-code"}, {ID: "minimax-m3"}, {ID: "qwen3-coder"}, {ID: "Doubao-Seed-2.1-Pro"},
}

// dynamicModelsCache 保留给旧测试/旧单平台语义；实际多平台缓存放在 Runtime 内。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	out := []map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		if rt.Pool == nil || len(rt.Pool.List()) == 0 { // 只暴露已接入账号的平台
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		for _, mi := range infos {
			id := k.String() + "/" + mi.ID
			entry := map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": k.String()}
			if mi.ContextWindow > 0 {
				entry["context_length"] = mi.ContextWindow
			}
			if mi.MaxTokens > 0 {
				entry["max_output_tokens"] = mi.MaxTokens
			}
			out = append(out, entry)
		}
		// 虚拟 cheapest 模型：让客户端模型下拉框可直接选（请求时解析为最低费率模型）
		out = append(out, map[string]any{
			"id": k.String() + "/cheapest", "object": "model",
			"created": 1753600000, "owned_by": k.String(),
		})
	}
	return out
}

func (h *Handler) fetchRuntimeModels(rt *Runtime) []provider.ModelInfo {
	if rt.Kind == provider.WorkBuddy { // 兼容旧单平台缓存观察点
		dynamicModelsCache.RLock()
		if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
			out := dynamicModelsCache.ids
			dynamicModelsCache.RUnlock()
			return out
		}
		if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
			dynamicModelsCache.RUnlock()
			return nil
		}
		dynamicModelsCache.RUnlock()
	}
	rt.mu.RLock()
	if len(rt.models) > 0 && time.Since(rt.fetched) < dynamicModelsTTL {
		out := rt.models
		rt.mu.RUnlock()
		return out
	}
	if rt.Kind != provider.WorkBuddy && !rt.lastFail.IsZero() && time.Since(rt.lastFail) < modelsFetchFailCooldown {
		rt.mu.RUnlock()
		return nil
	}
	rt.mu.RUnlock()
	acct := rt.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := rt.Upstream.FetchModels(acct)
	if err != nil {
		// 401 自愈（对齐上游 wild-work 7dac299）：
		// 取模型用的这个账号 token 可能刚好过期，此时**先刷新再重试一次**，
		// 而不是直接判失败。否则模型/费率列表会长时间拿不到
		// —— 且失败会被记进 lastFail，导致后续 modelsFetchFailCooldown
		// 内一律返回空（表现为"费率表冷启动消失"）。
		var ue *provider.Error
		if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
			log.Printf("fetch models got 401 uid=%s，尝试刷新 token 后重试", acct.UID)
			if rerr := rt.Upstream.RefreshToken(acct); rerr == nil {
				_ = acct.SaveAtomic()
				infos, err = rt.Upstream.FetchModels(acct)
			} else {
				log.Printf("fetch models refresh failed uid=%s err=%v", acct.UID, rerr)
			}
		}
	}
	if err != nil || len(infos) == 0 {
		now := time.Now()
		rt.mu.Lock()
		rt.lastFail = now
		rt.mu.Unlock()
		if rt.Kind == provider.WorkBuddy {
			dynamicModelsCache.Lock()
			dynamicModelsCache.lastFail = now
			dynamicModelsCache.Unlock()
		}
		if err != nil {
			// 显式日志：此前失败完全静默，运维无法区分
			// 「上游没返回模型」与「token 失效」。
			log.Printf("fetch models failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
		}
		return nil
	}
	now := time.Now()
	rt.mu.Lock()
	rt.models = infos
	rt.fetched = now
	rt.lastFail = time.Time{}
	rt.mu.Unlock()
	if rt.Kind == provider.WorkBuddy { // 兼容旧测试观察点
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = now
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	return infos
}

// modelRate 从定价缓存中查询指定模型费率。返回 -1 表示未知（保守处理，视为付费模型）。
func (h *Handler) modelRate(kind provider.Kind, model string) float64 {
	if h.cfg.PricingFunc == nil {
		return -1
	}
	pricing := h.cfg.PricingFunc(kind)
	for _, p := range pricing {
		if p.Model == model {
			return p.Rate
		}
	}
	return -1
}

// MaxRequestBody 请求体上限。
//
// 超限必须**明确回 413**，不能静默截断：截断后 JSON 不再合法，
// 解析失败会误报成 invalid_model（把"请求太大"指向模型名格式），
// 排查时极具误导性（上游 wild-work issue #30）。
//
// 口径 32MiB（对齐上游 0bf1f4c）：多模态大图 base64 后膨胀约 33%，
// 8MiB 仅够约 6MB 原图 —— 会误伤正常的图片请求。
// 32MiB 可容纳约 24MB 原图，更符合多模态场景。
const MaxRequestBody = 32 << 20

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 先探一次是否超限，超限直接 413 —— 不做截断。
	//
	// 用 LimitReader 多读 1 字节判断是否越界：读到 N+1 说明实际超过 N。
	probe := make([]byte, MaxRequestBody+1)
	n, _ := io.ReadFull(r.Body, probe)
	if n > MaxRequestBody {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("request body exceeds %d bytes", MaxRequestBody))
		return
	}
	body := probe[:n]
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	// JSON 解析失败必须显式报出，不能静默吞掉后拿空 model 去做路由
	// （否则会把"请求体坏了"误报成 invalid_model）。
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"malformed JSON body: "+err.Error())
		return
	}
	rt, model, err := h.runtimeForModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 判断模型是否为 0 费率（免费）：
	//   - 付费模型（rate>0 或未知）：仅高积分账号可用
	//   - 免费模型（rate==0）：高积分与低积分账号均可用，优先消耗低积分账号
	rate := h.modelRate(rt.Kind, model)
	freeModel := rate == 0

	tried := map[string]bool{}
	var lastErr error
	// 最后一次上游错误的原始 status+body：候选耗尽时原样透传（而非包装成 503），
	// 客户端才能看到 6004 重置时间等上游细节。
	var lastStatus int
	var lastBody []byte
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickWithSticky(rt, freeModel, model)
		if acct == nil {
			break
		}
		if tried[acct.UID] {
			// 粘性路由选回已尝试的账号，清除粘性记录后降级重选
			h.stickyClear(rt)
			if freeModel {
				acct = rt.Pool.PickLowCreditForModel(tried, model)
				if acct == nil {
					acct = rt.Pool.PickExcludingForModel(tried, model)
				}
			} else {
				acct = rt.Pool.PickExcludingForModel(tried, model)
			}
			if acct == nil {
				break
			}
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if acct.NeedsRefresh(h.refreshSkewFor(rt.Kind)) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				h.stickyClear(rt)
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := rt.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.stickyClear(rt)
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			kind := rt.Upstream.Classify(status, string(respBody))
			h.applyUpstreamError(rt, acct.UID, kind, freeModel, respBody, model)
			h.stickyClear(rt)
			lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
			// 请求级错误（内容拦截 / 上下文超限等）：换任何账号都必然复现，
			// 立刻透传上游原文，不再无效轮转消耗其他账号配额。
			if !kind.Rotatable() {
				log.Printf("transparent platform=%s uid=%s kind=%s status=%d body=%s",
					rt.Kind, acct.UID, kind, status, truncateBody(respBody))
				transparentError(w, status, respBody)
				return
			}
			lastStatus = status
			lastBody = respBody
			continue
		}
		defer rc.Close()
		if peek.Stream {
			// 流式：先透传，成功后才记成功。
			// 注意 Stream 可能在【写成 HTTP 200 之后】才失败（见下），
			// 因此这里不能先 NoteSuccess —— 否则账号已被标记健康，
			// 而实际这次请求是失败的（H3/M2）。
			if serr := rt.Upstream.Stream(w, rc); serr != nil {
				// 上游在 200 body 里返回了非 SSE 错误（Stream 负责嗅探并判定），
				// 或流中途断开。此时响应头可能已发出，无法再回写错误码，
				// 但**必须**把错误反映到账号状态，否则后续请求会继续选中它。
				if ue, ok := serr.(*provider.Error); ok {
					log.Printf("stream error platform=%s uid=%s kind=%s msg=%s",
						rt.Kind, acct.UID, ue.Kind, ue.Msg)
					h.applyUpstreamError(rt, acct.UID, ue.Kind, freeModel, []byte(ue.Msg), model)
				} else {
					log.Printf("stream i/o error platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
				}
				h.stickyClear(rt)
				return
			}
			rt.Pool.NoteSuccess(acct.UID)
			h.stickySuccess(rt)
			return
		}
		resp, err := rt.Upstream.Aggregate(rc)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// 候选耗尽但轮换中确有上游错误：优先透传最后一个上游原始响应，
	// 保持上游语义（如 429 + 6004 重置时间）。本次请求确实逐号尝试过，
	// 包装成 503 反而丢信息，客户端不知道何时可重试。
	if lastStatus > 0 && lastBody != nil {
		transparentError(w, lastStatus, lastBody)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// applyUpstreamError 按错误类别更新账号池状态（冷却/禁用/计数）。
//
// 从 chatCompletions 的错误分支中抽出，供两条路径共用：
//   - 非 2xx 响应（走 Classify）
//   - 流式透传中途发现的上游错误（Stream 嗅探出 *provider.Error，H3）
//
// 后者此前会被完全忽略（`_ = Stream(...)`），导致：
//  1. 账号仍被标记为健康（NoteSuccess 在 Stream 之前调用）；
//  2. 后续请求继续选中这个实际不可用的账号。
//
// freeModel 表示本次请求是否为 0 费率（免费）模型，用于区分「免费额度用完」的
// 惩罚力度：免费模型失败只需换号重试，不该把账号冷却到次日。
// body 为上游原始响应体（可能为 nil）：仅用于 429 时解析 6004 的真实重置时间；
// 不影响其他错误类别的处理。
// model 为本次请求的模型键：仅用于 429 时做【模型级冷却】；
// 为空时退化为整号冷却。
func (h *Handler) applyUpstreamError(rt *Runtime, uid string, kind provider.ErrKind, freeModel bool, body []byte, model string) {
	switch kind {
	case provider.ErrHardCredit:
		// 依据账号实际状态，而非模型费率，判断是否属于「免费额度已用完」：
		// 低积分账号即使调用 0 费率模型仍报余额不足。
		st, ok := rt.Pool.Status(uid)
		switch {
		case ok && st.LowCredit && freeModel:
			// 低积分账号调免费模型失败：不能冷却到次日 0 点（那会让该账号当天
			// 连免费模型都彻底吃不到，表现为「0 积分免费的也不能用」）。
			// 改为短冷却换号重试，稍后仍可重新参与轮转。
			rt.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "免费模型额度受限，换号重试")
		case ok && st.LowCredit:
			now := time.Now()
			until := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
			rt.Pool.Cooldown(uid, pool.CoolHard, until.Sub(now), "免费额度已用完，次日恢复")
		default:
			rt.Pool.Cooldown(uid, pool.CoolHard, h.cfg.HardCooldown, "余额/权益不足")
		}
	case provider.ErrSoftRate:
		// 429/6004 上游常按【模型】限额（响应里会提示"您也可以切换其他模型继续使用"），
		// 此时账号整体仍健康 —— 只冷却撞墙的模型，同号其他模型继续参与选号。
		// 若短时间内第二个模型也撞墙，CooldownModel 会升级为整号冷却
		// （防上游其实是按账号计时，逐个模型试探会拖垮账号）。
		//
		// 若响应含 6004「将在 <ts> 重置」，按真实重置点冷却；否则回退默认 SoftCooldown。
		d := h.softRateCooldown(string(body))
		if model != "" {
			if escalated := rt.Pool.CooldownModel(uid, model, d, "429 rate limit"); escalated {
				log.Printf("model cooldown escalated to account platform=%s uid=%s model=%s",
					rt.Kind, uid, model)
			} else {
				log.Printf("model cooldown platform=%s uid=%s model=%s", rt.Kind, uid, model)
			}
		} else {
			rt.Pool.Cooldown(uid, pool.CoolSoft, d, "429 rate limit")
		}
	case provider.ErrSessionDead:
		rt.Pool.Disable(uid, "session dead")
	case provider.ErrNotFound:
		rt.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")

	// ── 与账号无关：不冷却、不计数、不熔断 ──
	//
	// 这三类是「请求本身的问题」，换任何账号结果相同。
	// 对它们调用 NoteError 会把健康账号冷却掉（连续 3 次 → 10 分钟），
	// 并让调用方白白轮转、消耗其他账号的请求配额。
	case provider.ErrContentBlocked:
		log.Printf("upstream content blocked uid=%s（内容策略拦截，与账号无关，不罚账号）", uid)
	case provider.ErrPromptTooLong:
		log.Printf("upstream prompt too long uid=%s（上下文超限，请求级错误，不罚账号）", uid)
	case provider.ErrPassthrough:
		// 模型级限流（issue #53）：账号本身健康，只是当前模型暂不可用。
		// 不冷却账号，让客户端按 Retry-After 自退避或换模型 ——
		// 若在此冷却账号，该账号其它可用模型会被一起拖垮。
		log.Printf("upstream model-level rate limit uid=%s（请求级透传，不罚账号）", uid)
	case provider.ErrBadParams:
		log.Printf("upstream bad params uid=%s（请求体问题，不罚账号）", uid)

	// ── 账号级故障：需冷却轮换 ──
	case provider.ErrWafBlock:
		rt.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "WAF 拦截")
	case provider.ErrAccountFault:
		// 账号自身授权/配额故障（如 11140 需重新 OAuth），
		// 计错误并按阈值冷却，避免持续给上游送死请求。
		rt.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	case provider.ErrModelBlocked:
		// 该账号在此模型上不可用 → 计错误并轮转到他号。
		rt.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)

	default:
		rt.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("model must use explicit prefix: workbuddy/<model> or traework/<model>")
	}
	kind := provider.Kind(parts[0])
	rt := h.cfg.Runtimes[kind]
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", kind)
	}
	if len(rt.Pool.List()) == 0 {
		return nil, "", fmt.Errorf("provider %q has no account", kind)
	}
	modelName := parts[1]
	// cheapest 模型：选择费率最低的模型。
	// 注意不用 "auto"——与 WorkBuddy 上游原生 auto 模型（平台智能路由）撞名，
	// 本地拦截会让上游 auto 永远调不到；auto 现在直通上游。
	if strings.EqualFold(modelName, "cheapest") {
		resolved := h.resolveAutoModel(kind)
		if resolved != "" {
			log.Printf("cheapest model resolved platform=%s from=%s to=%s", kind, modelName, resolved)
			modelName = resolved
		}
	}
	return rt, modelName, nil
}

// resolveAutoModel 从定价缓存中选择费率最低的模型。
// 返回空字符串时表示无法确定（调用方保留原模型名）。
func (h *Handler) resolveAutoModel(kind provider.Kind) string {
	if h.cfg.PricingFunc == nil {
		return ""
	}
	pricing := h.cfg.PricingFunc(kind)
	if len(pricing) == 0 {
		return ""
	}

	// 候选白名单过滤
	allowList := h.cfg.AutoModels[kind]
	allowMap := make(map[string]bool, len(allowList))
	for _, m := range allowList {
		allowMap[m] = true
	}

	var best *provider.ModelPricing
	for _, p := range pricing {
		if p.Rate <= 0 {
			continue // 非计费模型（含上游原生 auto）不参与
		}
		if len(allowMap) > 0 && !allowMap[p.Model] {
			continue // 不在白名单中
		}
		if best == nil || p.Rate < best.Rate {
			best = &provider.ModelPricing{Model: p.Model, Rate: p.Rate, Channel: p.Channel}
		}
	}
	if best == nil {
		return ""
	}
	log.Printf("cheapest model picked platform=%s model=%s rate=%.2f", kind, best.Model, best.Rate)
	return best.Model
}

func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	// 修补工具轮的空 content（见 normalizeToolTurnContent）后再覆盖 model。
	normalizeToolTurnContent(obj)
	obj["model"] = model
	return json.Marshal(obj)
}

// normalizeToolTurnContent 把「content 为 null 或缺失」的工具轮消息补成空串。
//
// 上游要求带 tool_calls 的 assistant 消息必须携带**字符串** content；
// null 或字段缺失会让它**整请求拒答**，并且给一个完全误导的错：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 实际与 tool 配对无关（对齐上游 wild-work 1b38ac1 / PR #51 的实测）。
//
// 危害在于**中毒进历史**：模型某轮只回工具调用、没有正文时，客户端会把该
// assistant 消息的 content 落成 null，之后这一轮永远留在 messages 里 ——
// 于是该会话的**每一发**请求都被上游拒，重启客户端也不恢复，只能新建会话。
//
// 客户端侧看到的是 200 + 无 choices 的错误帧（表现为"空流"），
// 排查方向会被带偏到 tool 配对上。
func normalizeToolTurnContent(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			// 只在真的带 tool_calls 时补：
			// 普通 assistant 消息 content=null 无证据会出问题，不擅自改写。
			if tc, ok := m["tool_calls"].([]any); !ok || len(tc) == 0 {
				continue
			}
			if v, exists := m["content"]; !exists || v == nil {
				m["content"] = ""
			} else if _, isStr := v.(string); !isStr {
				m["content"] = ""
			}
		case "tool":
			// 工具返回空内容时客户端同样可能给 null，上游是同一套校验。
			if v, exists := m["content"]; !exists || v == nil {
				m["content"] = ""
			} else if _, isStr := v.(string); !isStr {
				m["content"] = ""
			}
		}
	}
}

func (h *Handler) runtimeKinds() []provider.Kind {
	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes))
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// transparentError 把上游响应【原样】回写给客户端（保留上游 status 与 body）。
//
// 用于两类场景（移植自上游 wild-work PR #82 的思路）：
//  1. 请求级错误（内容拦截 / 上下文超限）：换任何账号都必然复现，
//     应立刻把上游原文给客户端，而不是轮转到耗尽后包装成 503；
//  2. 候选全部耗尽：本次确实逐号尝试过，包装成 503 会丢掉上游细节
//     （如 429 + 6004 重置时间），客户端因此无法知道何时可重试。
//
// softRateResetMaxCap 解析出的重置时长上限：
// 上游时间戳异常（如错给一年后）时不至于把号冻死。
const softRateResetMaxCap = 24 * time.Hour

// softRateCooldown 从限流响应体提取【真实重置时间】，冷却到该时刻
// （而非固定 SoftCooldown）。
//
// 移植自上游 wild-work PR #82（d4314a2）。
//
// 命中形态：WorkBuddy 6004「...将在 2026-09-23 14:47:19 UTC+8 重置...」
//
// 为什么必要：该类限流按重置点解封，固定 60s 软冷却过期后账号会被
// 反复选中、反复 429 —— 表现为"解禁了又限流"的循环。
// 解析失败或时长为非正值时回退默认 SoftCooldown（不改变原有行为）。
func (h *Handler) softRateCooldown(body string) time.Duration {
	ts, ok := softRateResetTs(body)
	if !ok {
		return h.cfg.SoftCooldown
	}
	d := time.Until(ts)
	if d <= 0 {
		return h.cfg.SoftCooldown
	}
	if d > softRateResetMaxCap {
		d = softRateResetMaxCap
	}
	return d
}

// softRateResetTs 提取「将在 <ts> 重置」中的时间戳（UTC+8 墙钟）；未命中返回 false。
func softRateResetTs(body string) (time.Time, bool) {
	const marker = "将在 "
	i := strings.Index(body, marker)
	if i < 0 {
		return time.Time{}, false
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, " 重置")
	if end < 0 {
		return time.Time{}, false
	}
	s := strings.TrimSpace(rest[:end])
	// 「UTC+8」是时区说明而非时间的一部分（layout 无法解析），剥掉
	if suffix := " UTC+8"; strings.HasSuffix(s, suffix) {
		s = strings.TrimSuffix(s, suffix)
	}
	const layout = "2006-01-02 15:04:05"
	loc := time.FixedZone("UTC+8", 8*3600)
	reset, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return reset, true
}

// truncateBody 日志用的响应体摘要（避免超长 body 淹没日志）。
func truncateBody(body []byte) string {
	const max = 200
	s := string(body)
	if len(s) > max {
		return s[:max]
	}
	return s
}

func transparentError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

func WorkBuddyStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, workbuddyStaticModels...)
}
func TraeWorkStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, traeworkStaticModels...)
}

// statsSnapshot 运行统计快照（GET /api/stats）。
//
// Stats 为 nil（初始化失败）时返回 503 与明确原因 —— 统计是旁路能力，
// 不影响代理主流程，接口只是如实告知不可用。
func (h *Handler) statsSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Stats == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "stats_unavailable",
			"运行统计引擎未初始化（不影响代理转发）")
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Stats.Snapshot(time.Now()))
}

// ledgerQuery 双流水账本（GET /api/ledger?days=1|7）。
//
// days 非 1 时按 7 天处理（与 ledger.Query 同口径）。
// Ledger 为 nil 时返回 503。
//
// 注意：ledger 与 stats 刻意不合并（上游 R43）—— 差值口径含积分包到期作废，
// ledger 按 spend/expire 拆分，两套数字天然不同。
func (h *Handler) ledgerQuery(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ledger == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "ledger_unavailable",
			"账本未初始化（不影响代理转发）")
		return
	}
	days := 7
	if r.URL.Query().Get("days") == "1" {
		days = 1
	}
	// enrich：uid → (昵称, 渠道)，遍历各渠道池补齐展示信息
	enrich := func(uid string) (string, string) {
		for k, rt := range h.cfg.Runtimes {
			if rt == nil || rt.Pool == nil {
				continue
			}
			if st, ok := rt.Pool.Status(uid); ok {
				return st.Nickname, k.String()
			}
		}
		return "", ""
	}
	writeJSON(w, http.StatusOK, h.cfg.Ledger.Query(days, enrich))
}
