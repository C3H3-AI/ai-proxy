// CODE GENERATED FROM wild-work@c62d0bc -- DO NOT EDIT, run sync_vendor.sh

// solosse.go SOLO 自定义 SSE 解析 → OpenAI SSE（流式转换 + 非流式聚合）。
//
// SOLO 事件序列（SPEC §4.6，实测）：
//
//	id:1
//	event:metadata
//	data:{"model":"","session_id":"...","prompt_completion_id":0,...}
//
//	id:2
//	event:timing_cost
//	data:{"name":"llm_raw_chat_v2",...}
//
//	event:output                          ← ×N，核心内容
//	data:{"response":"<content 增量>",
//	      "reasoning_content":"<思考链增量>",
//	      "tool_calls":<null 或工具调用>}
//
//	event:extra_info                       ← 含 reasoning_content 完整版
//	event:token_usage
//	data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
//
//	event:done
//	data:{"finish_reason":"stop"}
package traework

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// SOLOEvent 单条 SOLO SSE 事件（归一化）。
type SOLOEvent struct {
	Event        string          // metadata | timing_cost | output | extra_info | token_usage | done | error
	Response     string          // output: content 增量
	Reasoning    string          // output: 思考链增量
	ToolCalls    json.RawMessage // output: 工具调用（null 或对象/数组）
	Usage        map[string]any  // token_usage
	FinishReason string          // done
	ErrorCode    int64           // error
	ErrorMessage string          // error
}

// SOLOStreamError 上游 SSE 流内的业务错误（event:error）。非流式聚合时返回，
// 调用方可据此分类冷却账号并轮转。
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

// Kind 将 SSE 流内错误分类。1005 → provider.ErrHardCredit；其余归 provider.ErrClient。
func (e *SOLOStreamError) Kind() provider.ErrKind {
	switch e.Code {
	case 1005:
		// 权益/余额不足 → 硬冷却
		return provider.ErrHardCredit
	case 1001:
		// 模型不可用 → 按模型避让（请求级，不罚账号整体）
		return provider.ErrModelBlocked
	case 3004, 9074:
		// 上游限流 → 软冷却换号。
		// 对齐上游映射：3004/9074 若不识别会落到 ErrClient（罚账号+计错），
		// 而它们实际只是限流，应短冷却后换号。
		return provider.ErrSoftRate
	}
	return provider.ErrClient
}

// ParseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// sseState 维护一行 SSE 的 event/data 跨行累积。
type sseState struct {
	event string
	data  strings.Builder
}

// reset 清空状态（事件边界触发）。
func (s *sseState) reset() {
	s.event = ""
	s.data.Reset()
}

// scanLine 处理一行；返回该行触发的事件（事件边界时解析并返回）。
func scanLine(st *sseState, line string) *SOLOEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := ParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// Aggregate 读取完整 SOLO SSE，聚合 response + reasoning + tool_calls + usage，
// 产出单个 OpenAI chat.completion（非流式）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id           string
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		toolSeq      int
		idIndex      = map[string]int{}
		upstreamErr  error
	)
	st := &sseState{}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "metadata":
				if id == "" && ev.Usage != nil {
					// metadata 无 id 可用，保留为空，末尾补 chatcmpl。
				}
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				mergeToolCallJSON(toolCalls, &toolOrder, ev.ToolCalls, &toolSeq, idIndex)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallJSON 把 SOLO output.tool_calls（json.RawMessage，可能 null/对象/数组）
// 合并进 toolCalls（按 index）。
// mergeToolCallJSON 合并一批 tool_call 分片。
//
// index 分派顺序（与 upstream.Aggregate 同口径）：
//  1. 显式 index 优先；
//  2. 否则按 id 复用已分配的 index，未见过的 id 则跳号新分配；
//  3. 否则沿用上一个 index（流式分片语义：同一条目的后续片段常不带 index/id）；
//  4. 都没有则跳号新分配。
//
// ⚠️ 原实现把「缺 index」一律当 0，导致一批互不相关的 tool_call
// 全部挤进 index 0 互相覆盖 —— tool_call 数量变少、arguments 拼接错乱。
func mergeToolCallJSON(toolCalls map[int]map[string]any, toolOrder *[]int, raw json.RawMessage,
	toolSeq *int, idIndex map[string]int) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	nextIdx := func() int {
		for {
			i := *toolSeq
			*toolSeq++
			if _, used := toolCalls[i]; !used {
				return i
			}
		}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := -1
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		} else if cid, _ := call["id"].(string); cid != "" {
			if mid, seen := idIndex[cid]; seen {
				idx = mid
			} else {
				idx = nextIdx()
			}
		} else if len(*toolOrder) > 0 {
			idx = (*toolOrder)[len(*toolOrder)-1]
		} else {
			idx = nextIdx()
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		if cid, _ := call["id"].(string); cid != "" {
			idIndex[cid] = idx
		}
		mergeToolCallDelta(merged, call)
		if cid, _ := merged["id"].(string); cid != "" {
			idIndex[cid] = idx
		}
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
// 上游 SOLO 用 `function_call` 字段（实测），OpenAI 标准用 `function`；两者都兼容。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		df, _ = delta["function_call"].(map[string]any) // SOLO 专属字段名
	}
	if df == nil {
		return
	}
	// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
	delete(df, "namespace")
	delete(df, "partial_arguments")
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// Stream 流式转换：SOLO SSE → OpenAI SSE chunk，每 chunk flush，保证至少一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// Stream 流式转发。
//
// 流内业务错误（event:error）不能伪装成正常收尾：此前只把错误描述写进
// delta.content 并补 [DONE]，导致
//  1. 客户端把失败的流当成完整回答（半截内容 + 正常结束）；
//  2. handler 收不到 error，账号**不会**被记入失败状态 ——
//     多账号聚合里该账号会继续被选中并再次失败。
//
// 现在改为：以 OpenAI 规范 error 帧收尾（**不补 [DONE]**），
// 并把错误按 Kind() 分类返回给 handler，由它更新账号池状态。
func Stream(w http.ResponseWriter, r io.Reader) error {
	var upErr *SOLOStreamError
	err := streamOpts(w, r, func(se *SOLOStreamError) { upErr = se })
	if err != nil {
		return err
	}
	if upErr != nil {
		return &provider.Error{
			Kind:   upErr.Kind(),
			Status: http.StatusOK, // 响应头已发出（200），仅用于分类
			Msg:    upErr.Error(),
		}
	}
	return nil
}

// StreamWithError 同 Stream，额外在遇到上游 event:error 时回调 onErr（非 nil），
// 供调用方冷却账号/记录日志；错误信息同时注入 SSE 事件流。
func StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError)) error {
	return streamOpts(w, r, onErr)
}

// streamOpts Stream 的可选参数版本。
func streamOpts(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError)) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var pendingUsage map[string]any
	sawDone := false
	st := &sseState{}
	writeChunk := func(delta map[string]any, finish string) error {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "",
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": delta,
				},
			},
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		if finish != "" {
			choice["finish_reason"] = finish
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeDONE := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]any
					if err := json.Unmarshal(ev.ToolCalls, &tc); err == nil {
						// SOLO 上游 tool_call 条目用 `function_call` 字段 → 转成 OpenAI 的 `function`
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]any); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
							if fn, ok := call["function"].(map[string]any); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
						}
						delta["tool_calls"] = tc
					}
				}
				if len(delta) > 0 {
					if err := writeChunk(delta, ""); err != nil {
						return err
					}
				}
			case "token_usage":
				pendingUsage = ev.Usage
			case "done":
				if err := writeChunk(map[string]any{}, ev.FinishReason); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			case "error":
				// 上游 SOLO 业务错误（1005 权益/1001 模型不可用/3004 限流等）。
				//
				// 以 OpenAI 规范 error 帧收尾，让客户端明确判失败；
				// **刻意不补 [DONE]** —— 补了会被当成正常结束，
				// 客户端把半截回答当完整回答继续跑（正是本修复要消灭的症状）。
				se := &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
				if onErr != nil {
					onErr(se)
				}
				errFrame, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": se.Error(),
						"type":    "upstream_error",
						"code":    fmt.Sprintf("solo_%d", ev.ErrorCode),
					},
				})
				if _, err := io.WriteString(w, "data: "+string(errFrame)+"\n\n"); err != nil {
					return err
				}
				if fl != nil {
					fl.Flush()
				}
				// 标记已收尾，避免下面的兜底再补一个 [DONE]
				sawDone = true
			}
		}
		if err == io.EOF {
			break
		}
	}
	if !sawDone {
		// 幂等兜底：上游中断（无 done）仍写 [DONE]。
		return writeDONE()
	}
	return nil
}

func jsonEscape(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
