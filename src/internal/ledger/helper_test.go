// 测试辅助：JSONL 行拆分与 JSON 解码的薄包装。
package ledger

import (
	"bytes"
	"encoding/json"
)

// splitLines 按行拆分（兼容 \n 与 \r\n）。
func splitLines(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// unmarshal JSON 解码薄包装（测试里少写 import）。
func unmarshal(line []byte, v any) error { return json.Unmarshal(line, v) }
