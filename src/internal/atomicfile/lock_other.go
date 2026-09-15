//go:build !unix

package atomicfile

// lockPath 在非 unix 平台退化为无操作。
//
// addon 运行时是 linux（alpine），此分支仅为保证
// `GOOS=windows go build` 等交叉编译/IDE 场景可编译。
// 唯一临时文件名已足以避免 rename 竞争，此处降级不影响正确性。
func lockPath(string) (func(), error) {
	return func() {}, nil
}
