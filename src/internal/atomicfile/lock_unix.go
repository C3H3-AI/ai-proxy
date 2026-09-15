//go:build unix

package atomicfile

import (
	"os"
	"syscall"
)

// lockPath 对 lockPath 指向的文件加**跨进程**排他锁（flock）。
//
// 为什么用 flock 而不是进程内 sync.Mutex：
// 同一份凭证会被 serverd 与 ctl 两个**独立进程**写入，
// 进程内锁对彼此完全不可见。
//
// 返回的 unlock 保证幂等：重复调用安全。
func lockPath(lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
