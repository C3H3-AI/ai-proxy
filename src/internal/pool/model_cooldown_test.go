package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// newPoolWith 构造含指定账号的池（不落盘）。
func newPoolWith(as ...*auth.Auth) *Pool {
	p := New("")
	for _, a := range as {
		p.Add(a)
	}
	return p
}

// TestModelCooldownDoesNotBlockOtherModels —— 核心价值：
// 429 按模型限额时，只冷却撞墙的模型；同号【其他模型】仍可选到该账号。
//
// 这正是移植模型级冷却的目的：旧实现整号冷却，会浪费账号在其他模型上的配额。
func TestModelCooldownDoesNotBlockOtherModels(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})
	a := p.PickForModel("model-A")
	if a == nil {
		t.Fatal("初始应能选到账号")
	}

	// 对 model-A 触发 429 模型级冷却
	if escalated := p.CooldownModel("u1", "model-A", 5*time.Minute, "429"); escalated {
		t.Fatal("只冷却 1 个模型时不应升级整号")
	}

	// 该账号对 model-A 不可选
	if got := p.PickForModel("model-A"); got != nil {
		t.Errorf("model-A 处于模型级冷却，不应选到该账号，实际 uid=%v", got.UID)
	}
	// 但对 model-B 仍可选（账号整体健康）
	got := p.PickForModel("model-B")
	if got == nil {
		t.Error("model-B 未被冷却，应仍能选到该账号（同号其他模型不受拖累）")
	} else if got.UID != "u1" {
		t.Errorf("应仍选到 u1，实际=%s", got.UID)
	}
}

// TestModelCooldownDoesNotBlockAccountLevel —— 模型级冷却不应把账号标记为
// Cooling/Disabled（面板与状态接口据此显示）。
func TestModelCooldownDoesNotBlockAccountLevel(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.CooldownModel("u1", "model-A", 5*time.Minute, "429")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在于池中")
	}
	if st.Cooling {
		t.Error("模型级冷却不应让账号 Cooling=true")
	}
	if st.Disabled {
		t.Error("模型级冷却不应禁用账号")
	}
	if st.ModelCooling == nil || len(st.ModelCooling) != 1 {
		t.Errorf("ModelCooling 应含 1 个条目，实际=%v", st.ModelCooling)
	}
	if _, has := st.ModelCooling["model-A"]; !has {
		t.Errorf("ModelCooling 应包含 model-A，实际=%v", st.ModelCooling)
	}
}

// TestModelCooldownEscalatesOnSecondModel —— 升级规则：
// 短时间内第二个模型也撞墙 → 升级为整号冷却（防上游按账号计时逐个试探）。
func TestModelCooldownEscalatesOnSecondModel(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})

	if escalated := p.CooldownModel("u1", "model-A", 5*time.Minute, "429"); escalated {
		t.Fatal("第一个模型不应升级")
	}
	// 第二个模型撞墙 → 应升级
	escalated := p.CooldownModel("u1", "model-B", 3*time.Minute, "429")
	if !escalated {
		t.Fatal("第二个模型撞墙时应升级为整号冷却")
	}

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Error("升级后账号应为 Cooling=true")
	}
	if len(st.ModelCooling) != 0 {
		t.Errorf("升级后应清空模型级冷却，实际=%v", st.ModelCooling)
	}
	// until 应取两个模型中最晚的解冻时刻
	if st.Until.IsZero() {
		t.Error("升级后应设置 until")
	}
	if st.Reason == "" {
		t.Error("升级后应保留 reason 便于排查")
	}
}

// TestModelCooldownExpiryFreesModel —— 模型冷却到期后该模型重新可选。
func TestModelCooldownExpiryFreesModel(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})
	// 极短冷却，等其过期
	p.CooldownModel("u1", "model-A", 30*time.Millisecond, "429")
	if got := p.PickForModel("model-A"); got != nil {
		t.Error("冷却期内不应选到该账号")
	}
	time.Sleep(60 * time.Millisecond)
	if got := p.PickForModel("model-A"); got == nil {
		t.Error("冷却到期后应重新可选")
	}
}

// TestModelCooldownPrunesExpiredEntries —— 过期条目应被清理，不无限增长。
func TestModelCooldownPrunesExpiredEntries(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})
	// 先加一个立刻过期的条目（直接写 entry，避免第二个模型触发升级）
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCool = map[string]time.Time{
		"stale": time.Now().Add(-time.Hour),
	}
	p.mu.Unlock()

	// 再对一个模型冷却 → 应顺带清理过期条目
	p.CooldownModel("u1", "fresh", time.Minute, "429")
	st, _ := p.Status("u1")
	if _, has := st.ModelCooling["stale"]; has {
		t.Errorf("过期条目应被清理，实际=%v", st.ModelCooling)
	}
	if _, has := st.ModelCooling["fresh"]; !has {
		t.Errorf("新条目应保留，实际=%v", st.ModelCooling)
	}
}

// TestModelCoolingPersisted —— 模型级冷却应随 state.json 落盘并在重启后恢复。
func TestModelCoolingPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.CooldownModel("u1", "model-A", 10*time.Minute, "429")

	// 用新 Pool 从同一 state 文件加载，验证恢复
	p2 := New(fp)
	p2.load()
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后账号应仍在池中")
	}
	if st.ModelCooling == nil {
		t.Fatal("重启后 ModelCooling 应恢复")
	}
	if _, has := st.ModelCooling["model-A"]; !has {
		t.Errorf("重启后应保留 model-A 的冷却，实际=%v", st.ModelCooling)
	}
	// 该模型应不可选
	if got := p2.PickForModel("model-A"); got != nil {
		t.Error("重启后 model-A 仍应处于冷却")
	}
}

// TestPickExcludingForModelSkipsCooled —— 轮转选取也应跳过模型级冷却的账号。
func TestPickExcludingForModelSkipsCooled(t *testing.T) {
	p := newPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	p.CooldownModel("u1", "model-A", time.Minute, "429")

	got := p.PickExcludingForModel(nil, "model-A")
	if got == nil {
		t.Fatal("u2 未被冷却，应能选到")
	}
	if got.UID != "u2" {
		t.Errorf("应跳过被模型级冷却的 u1，实际=%s", got.UID)
	}
}

// TestPickWithEmptyModelUnchanged —— model 为空时行为与旧版一致（不过滤）。
func TestPickWithEmptyModelUnchanged(t *testing.T) {
	p := newPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.CooldownModel("u1", "model-A", 10*time.Minute, "429")
	// model="" 时不应被模型级冷却影响
	if got := p.PickForModel(""); got == nil {
		t.Error("model 为空时不应受模型级冷却影响")
	}
	if got := p.Pick(); got == nil {
		t.Error("Pick() 应与旧版一致，不受模型级冷却影响")
	}
}

// TestModelCoolSnapshotEmptyReturnsNil —— 空/全过期时返回 nil（避免 JSON 空对象）。
func TestModelCoolSnapshotEmptyReturnsNil(t *testing.T) {
	if got := modelCoolSnapshot(nil); got != nil {
		t.Errorf("nil 输入应返回 nil，实际=%v", got)
	}
	allExpired := map[string]time.Time{"a": time.Now().Add(-time.Hour)}
	if got := modelCoolSnapshot(allExpired); got != nil {
		t.Errorf("全过期应返回 nil，实际=%v", got)
	}
	future := map[string]time.Time{"a": time.Now().Add(time.Hour)}
	if got := modelCoolSnapshot(future); len(got) != 1 {
		t.Errorf("未来条目应保留，实际=%v", got)
	}
}

// TestHealthyForModel —— healthyForModel 的判定逻辑。
func TestHealthyForModel(t *testing.T) {
	now := time.Now()
	e := &entry{}
	if !e.healthyForModel(now, "m") {
		t.Error("无任何冷却时应对任意模型 healthy")
	}
	e.modelCool = map[string]time.Time{"m": now.Add(time.Hour)}
	if e.healthyForModel(now, "m") {
		t.Error("该模型在冷却中，应不 healthy")
	}
	if !e.healthyForModel(now, "other") {
		t.Error("其他模型应仍 healthy")
	}
	if !e.healthyForModel(now, "") {
		t.Error("model 为空应退化为 healthy")
	}
}
