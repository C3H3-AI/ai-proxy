// Package atomicfile 提供跨进程安全的原子文件写入。
//
// 背景（真实缺陷，非理论风险）：
//
// 原实现用【固定的】临时文件名 "path.tmp"：
//
//	tmp := path + ".tmp"
//	os.WriteFile(tmp, raw, 0o600)
//	os.Rename(tmp, path)
//
// 单进程内由互斥锁保护时没问题，但本项目的实际架构是**多进程**：
//
//	serverd —— 请求前预刷新 / 定时保活，写 auths/*.json
//	ctl     —— 独立子进程，面板点「刷新令牌」时也写同一文件
//
// 两个进程各有自己的内存副本与锁，**进程间无互斥**，于是：
//
//  1. 双方写同一个 tmp 文件 → 内容交错；
//  2. 一方先 rename 走，另一方 rename 时 tmp 已不存在 →
//     "no such file or directory"（实测并发 400 次出现 3 次）；
//  3. 更严重的是**逻辑丢失**：上游 refresh 会轮换 refreshToken，
//     两个进程各拿到一个新值，后落盘者覆盖先落盘者。
//     若落盘的是已被轮换失效的那个，账号下次刷新即失败，需人工重登。
//
// 文件本身不会损坏（rename 是原子的），丢的是"较新的那个值"。
//
// 本包的做法：
//   - 临时文件用 os.CreateTemp 生成**唯一名字**（消除写交错与 rename 竞争）；
//   - 写入前对目标文件路径加**跨进程文件锁**（flock），串行化同一路径的写入；
//   - rename 后 fsync 目录，保证崩溃后不丢已提交内容（尽力而为）。
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile 原子地把 data 写入 path（权限 perm）。
//
// 实现：同目录唯一临时文件 + 跨进程 flock + rename。
// 任何一步失败都会清理临时文件，绝不留半成品。
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomicfile: mkdir %s: %w", dir, err)
	}

	// 跨进程互斥：锁文件独立于目标文件，避免锁住 rename 本身。
	// 放在同一目录，保证与目标文件处于同一文件系统。
	unlock, err := lockPath(path + ".lock")
	if err != nil {
		// 取不到锁不致命：退化为"仅唯一临时文件"的无锁写入，
		// 至少不会出现原实现那种 rename 竞争导致的失败。
		unlock = func() {}
	}
	defer unlock()

	// 唯一临时文件：os.CreateTemp 保证并发调用互不冲突。
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp: %w", err)
	}
	tmp := f.Name()
	// 失败路径清理；成功后 tmp 已被 rename，Remove 是空操作。
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("atomicfile: write temp: %w", err)
	}
	// 先落盘再 rename：否则崩溃后可能 rename 了一个内容还在页缓存的文件。
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("atomicfile: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temp: %w", err)
	}
	// CreateTemp 默认 0600，按调用方要求收紧/放宽。
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("atomicfile: chmod temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomicfile: rename: %w", err)
	}

	// 目录 fsync：确保 rename 本身持久化（Linux 上必要）。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
