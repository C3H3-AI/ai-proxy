// CODE GENERATED FROM wild-work@c62d0bc -- DO NOT EDIT, run sync_vendor.sh

// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// errEmptyStream 表示上游 200 但一个有效 SSE 帧都没发（空流）。
// 调用方据此走错误分类/冷却，而不是当作成功。
var errEmptyStream = errors.New("upstream returned empty stream")

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		toolSeq       int            // 缺 index 时的跳号分配游标
		idIndex       = map[string]int{} // tool_call id → 分配的 index
	)

	// appendContent 是「已取到正文」的**唯一**写入点：空串不置 latch。
	// 若空 content 帧也置 gotAnyContent，后续 message 形态的正文
	// 会被 !gotAnyContent 判断挡住而**永久丢弃**（上游 issue #142）。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// nextToolIndex 分配一个未被占用的 index（跳号，避免与已有条目冲突）。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// 结束标记；无需额外处理（聚合在 EOF 收尾）
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										// index 分派（对齐上游 wb2api）：
										//   1) 显式 index 优先
										//   2) 否则按 id 复用/新分配
										//   3) 否则沿用上一个 index（沿用流式分片语义）
										//   4) 否则跳号新分配
										// 原实现把缺 index 一律当 0，会让多个 tool_call
										// 挤到同一条目上互相覆盖，参数错乱。
										idx := -1
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										} else if cid, _ := call["id"].(string); cid != "" {
											if mid, seen := idIndex[cid]; seen {
												idx = mid
											} else {
												idx = nextToolIndex()
											}
										} else if len(toolOrder) > 0 {
											idx = toolOrder[len(toolOrder)-1]
										} else {
											idx = nextToolIndex()
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
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
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									appendContent(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
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
		"created": int64(created),
		"model":   model,
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

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
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

// Stream 透传上游 SSE 到 w（每行 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
//
// 关于「200 里带错误」：上游并非只在 status>=400 时报错。
// 同包的 doJSONWith / FetchModels / FetchModelPricing 三处都显式处理了
// `code != 0` 的 200 响应（见 client.go:179 / 309 / 544），说明这是真实形态。
// 而流式路径此前只做逐行透传，若上游以 200 返回
//   {"code":1,"msg":"余额不足"}
// 这类**非 SSE 单行 JSON**，会被原样写给客户端：
//   - OpenAI SDK 收到非 `data: ` 前缀内容 → 解析失败或静默忽略；
//   - 且该账号不会被 Classify 判定 → 不冷却 → 后续请求继续选中它。
//
// 因此这里在透传前对**首个有效行**做嗅探：若它不是 SSE 帧（`data: ` / `event:` /
// 注释 `:` / 空行），则尝试按上游错误信封解析；判定为错误时返回 *Error，
// 由调用方走既有的错误分类与冷却逻辑。
func Stream(w http.ResponseWriter, r io.Reader) error {
	br := bufio.NewReaderSize(r, 64*1024)

	// ── 首帧嗅探 ──────────────────────────────────────────────
	// 只 peeking 第一行，不影响后续透传（peek 不消费）。
	if first, err := peekFirstNonEmptyLine(br); err == nil && first != "" {
		if kind, msg, isErr := classifyNonSSELine(first); isErr {
			log.Printf("chat_stream: upstream returned non-SSE error in 200 body: %s", truncate(msg, 200))
			return &Error{Kind: kind, Status: http.StatusOK, Msg: truncate(msg, 200)}
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// truncated 标记「可证实的截断」（对齐上游 wild-work issue #42）：
	//   - 读到一半遇到传输层错误（连接中断 / Timeout 掐断 body）；
	//   - 或最后一行没有换行符、且该行不是完整 JSON —— 说明是半个帧。
	// 两者都是流被从中间砍断。此前这里会**无条件补 data: [DONE]**，
	// 把半截流伪装成正常收尾 → 客户端拿残缺的 tool_call arguments 去解析，
	// 报 "tool input was not fully received"，而网关日志里什么都看不到。
	//
	// 注意区分「上游正常 EOF 但本就不发 [DONE]」：帧完整、有换行符，
	// 仍照旧兜底补 DONE —— 部分上游确实不发 DONE，
	// 把它误判为截断会误伤全部正常请求。
	truncated := false
	truncErr := ""
	validFrames := 0

	writeRaw := func(raw string) error {
		if _, werr := io.WriteString(w, "data: "+raw+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: [DONE]") {
				// 上游显式 DONE：以完整的 SSE 帧结束（末尾空行 \n\n），
				// 与兜底补 DONE 的形态保持一致 —— 否则客户端会认为帧未结束。
				if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil {
					return werr
				}
				if fl != nil {
					fl.Flush()
				}
				return nil
			}
			if strings.HasPrefix(trimmed, "data: ") {
				validFrames++
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
			// EOF 处未以换行收尾：这一行本身可能就是半个帧。
			// 若它不是完整 JSON，说明被砍断 —— 已写出的是残缺数据，必须报截断。
			if err == io.EOF && !strings.HasSuffix(line, "\n") && strings.TrimSpace(trimmed) != "" {
				var probe map[string]any
				if jsonErr := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &probe); jsonErr != nil {
					truncated = true
					truncErr = "upstream stream truncated (partial frame at EOF)"
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			// 传输层错误：可证实的截断
			truncated = true
			truncErr = "upstream stream truncated: " + err.Error()
			break
		}
	}

	// ── 收尾 ────────────────────────────────────────────────────
	if truncated {
		// 截断：发一帧 OpenAI 规范的 error，客户端据此明确判失败；**不补 [DONE]**。
		raw := fmt.Sprintf(`{"error":{"message":%s,"type":"upstream_error","code":"upstream_truncated"}}`,
			jsonString(truncErr))
		if werr := writeRaw(raw); werr != nil {
			return werr
		}
		log.Printf("chat_stream: %s (frames=%d)", truncErr, validFrames)
		// 带 ErrServer 分类返回：handler 会走 applyUpstreamError →
		// 默认分支 NoteError，让被截断的账号也进入冷却，
		// 否则后续请求会继续选中它（这正是 #42 要解决的问题）。
		return &Error{Kind: ErrServer, Status: http.StatusOK, Msg: truncErr}
	}
	if validFrames == 0 {
		// 空流：上游 200 但没发任何有效帧。补 DONE 让客户端能收尾，
		// 但返回错误让调用方做分类/冷却。
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		log.Printf("chat_stream: upstream returned empty stream (200, 0 data frames)")
		return &Error{Kind: ErrServer, Status: http.StatusOK, Msg: errEmptyStream.Error()}
	}
	// 正常 EOF（帧完整但上游漏发 DONE）：兜底补一个，保证恰好一个 [DONE]。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// jsonString 把字符串编成 JSON 字面量（用于构造本地 error 帧的 message 字段）。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"upstream error"`
	}
	return string(b)
}

// peekFirstNonEmptyLine 返回首个非空行的内容（**不消费**缓冲）。
//
// 用 Peek 而非 ReadString：嗅探失败时不能吃掉首行，否则正常流会丢帧。
//
// 实现要点：反复 Peek 整个已缓冲内容，扫描其中的首个非空行；
// 缓冲不足时用 Peek(n) 主动触发底层读取（n 单调递增，保证终止）。
func peekFirstNonEmptyLine(br *bufio.Reader) (string, error) {
	for n := 1; n <= 64*1024; {
		buf, err := br.Peek(n)
		// 在已缓冲数据中按行扫描，返回第一个非空行
		for rest := buf; ; {
			idx := bytes.IndexByte(rest, '\n')
			if idx < 0 {
				break // 这一行还没读全，扩大窗口
			}
			if line := strings.TrimSpace(string(rest[:idx])); line != "" {
				return line, nil
			}
			rest = rest[idx+1:] // 空行，继续看下一行
		}
		// 已缓冲部分全是空行且无换行结尾：若没有更多数据可读，返回空
		if err != nil {
			if len(buf) == 0 {
				return "", err
			}
			// 有内容但无换行（流结束），当作最后一行
			return strings.TrimSpace(string(buf)), nil
		}
		if n < len(buf) {
			n = len(buf) // 已缓冲多于请求量，直接对齐
		}
		n *= 2
	}
	return "", nil
}

// classifyNonSSELine 判断一行内容是否为「非 SSE 的错误载荷」。
//
// 返回 (错误类别, 原始文本, 是否确认为错误)。
// 合法的 SSE 行（`data: ` / `event: ` / `id: ` / `retry: ` / 注释 `:` / 空行）
// 一律返回 false，保证正常流不受影响。
func classifyNonSSELine(line string) (ErrKind, string, bool) {
	t := strings.TrimSpace(line)
	if t == "" {
		return ErrNone, "", false
	}
	// 合法 SSE 字段前缀
	for _, p := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if strings.HasPrefix(t, p) {
			return ErrNone, "", false
		}
	}
	// 非 SSE 行：尝试按上游错误信封解析
	var env apiEnvelope
	if err := json.Unmarshal([]byte(t), &env); err == nil {
		// code != 0 视为业务错误；走与 doJSONWith 相同的分类口径
		if env.Code != 0 {
			kind := Classify(http.StatusOK, env.Msg)
			if kind == ErrNone {
				kind = ErrClient
			}
			return kind, fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg), true
		}
		// code == 0 但仍非 SSE：形态异常
		return ErrClient, truncate(t, 200), true
	}
	// 非 JSON 且非 SSE —— 不是合法的 SSE 流内容
	return ErrClient, truncate(t, 200), true
}
