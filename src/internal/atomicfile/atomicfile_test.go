package atomicfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWriteFileConcurrentNoRenameRace 复现并锁定原始缺陷。
//
// 旧实现用固定临时名 "path.tmp"，并发写入时会出现：
//
//	rename .../auth.json.tmp .../auth.json: no such file or directory
//
// （一方先 rename 走，另一方 rename 时 tmp 已不存在）
//
// 实测旧实现：400 次并发写入出现 3 次错误。
// 本测试要求新实现零错误。
func TestWriteFileConcurrentNoRenameRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	const writers = 4
	const iters = 200

	var wg sync.WaitGroup
	errCh := make(chan error, writers*iters)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]any{
				"writer": id,
				"pad":    make([]byte, 4096), // 拉长写窗口，放大竞争
			})
			for i := 0; i < iters; i++ {
				if err := WriteFile(path, payload, 0o600); err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		t.Fatalf("%d/%d 次写入失败（旧实现会出现 rename 竞争错误）: 首个错误 = %v",
			len(errs), writers*iters, errs[0])
	}

	// 结果必须是完整合法的 JSON（不能是两次写入交错的产物）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("文件不是合法 JSON（写交错）: %v", err)
	}
	if _, ok := got["writer"]; !ok {
		t.Fatalf("内容异常: %v", got)
	}
}

// TestWriteFileNoStrayTempFiles 确认成功路径不留临时文件。
func TestWriteFileNoStrayTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")

	for i := 0; i < 20; i++ {
		if err := WriteFile(path, []byte(fmt.Sprintf(`{"i":%d}`, i)), 0o600); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 允许存在 .lock（跨进程锁文件），但不应残留 .tmp-*
	for _, e := range entries {
		name := e.Name()
		if name == "x.json" || name == "x.json.lock" {
			continue
		}
		t.Errorf("残留文件: %s", name)
	}
}

// TestWriteFilePerm 确认权限被正确设置（凭证文件必须 0600）。
func TestWriteFilePerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")

	if err := WriteFile(path, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("权限 = %o, 期望 600", got)
	}
}

// TestWriteFileOverwrite 确认覆盖写语义（旧内容被完整替换，不残留尾部）。
func TestWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "o.json")

	if err := WriteFile(path, []byte(`{"long":"aaaaaaaaaaaaaaaaaaaaaaaa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"s":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"s":1}` {
		t.Errorf("覆盖后内容 = %q（不应残留旧内容）", string(raw))
	}
}
