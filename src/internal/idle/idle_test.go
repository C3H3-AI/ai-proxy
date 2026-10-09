package idle

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingReader 模拟卡死的上游：第一次读吐出一点数据，之后一直阻塞，
// 直到 ctx 被 cancel 才返回错误（与真实 net.Conn 行为一致）。
type blockingReader struct {
	data     []byte
	off      int
	blocked  chan struct{} // 第一次读完后关闭
	ctx      context.Context
	closedCh chan struct{}
	once     sync.Once
}

func newBlockingReader(ctx context.Context, data string) *blockingReader {
	return &blockingReader{
		data:     []byte(data),
		blocked:  make(chan struct{}),
		ctx:      ctx,
		closedCh: make(chan struct{}),
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		if r.off >= len(r.data) {
			r.once.Do(func() { close(r.blocked) })
		}
		return n, nil
	}
	// 之后一直阻塞，直到 ctx 被 cancel
	r.once.Do(func() { close(r.blocked) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *blockingReader) Close() error {
	close(r.closedCh)
	return nil
}

// TestMonitorBreaksIdleStream —— 核心回归：上游卡死后，
// 监控应在 idle 阈值内主动断流（Read 返回错误），而不是无限期挂起。
func TestMonitorBreaksIdleStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rc := newBlockingReader(ctx, "data:")

	m := Monitor(cancel, rc, 60*time.Millisecond)

	// 先读到首段数据
	buf := make([]byte, 16)
	n, err := m.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("首次读取应成功，n=%d err=%v", n, err)
	}

	// 再读：上游已卡死，应在阈值内返回错误（而不是一直阻塞）
	done := make(chan error, 1)
	go func() {
		_, e := m.Read(buf)
		done <- e
	}()

	select {
	case e := <-done:
		if e == nil {
			t.Error("卡死流应返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("监控未在合理时间内断流（Read 仍阻塞）")
	}
}

// TestMonitorRefreshesOnData —— 有数据时不应误断流（读到数据即续命）。
func TestMonitorRefreshesOnData(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 每 30ms 吐一点数据，总时长超过 idle 阈值也不该被断
	pr, pw := io.Pipe()
	m := Monitor(cancel, pr, 150*time.Millisecond)

	go func() {
		for i := 0; i < 10; i++ {
			pw.Write([]byte("x"))
			time.Sleep(30 * time.Millisecond)
		}
		pw.Close()
	}()

	total := 0
	buf := make([]byte, 8)
	for i := 0; i < 10; i++ {
		n, err := m.Read(buf)
		if n > 0 {
			total += n
		}
		if err != nil {
			break
		}
	}
	m.Close()
	// 持续有数据时不应被 idle 监控掐断（应读满 10 个字节）
	if total != 10 {
		t.Errorf("有持续数据时应全部读完，实际读到 %d 字节（可能被误断）", total)
	}
}

// TestMonitorCloseIdempotent —— Close 必须幂等且释放底层流。
func TestMonitorCloseIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rc := newBlockingReader(ctx, "d")
	m := Monitor(cancel, rc, time.Second)

	if err := m.Close(); err != nil {
		t.Errorf("首次 Close 应成功：%v", err)
	}
	// 二次 Close 不应 panic（幂等）
	if err := m.Close(); err != nil {
		t.Errorf("二次 Close 应幂等返回 nil：%v", err)
	}
	select {
	case <-rc.closedCh:
	default:
		t.Error("Close 应关闭底层流")
	}
}

// TestMonitorZeroTimeoutUsesDefault —— 传入 0 或负数应回落默认值。
func TestMonitorZeroTimeoutUsesDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := newBlockingReader(ctx, "d")
	m := Monitor(cancel, rc, 0)
	b, ok := m.(*monitoringBody)
	if !ok {
		t.Fatalf("类型断言失败: %T", m)
	}
	if b.idle != DefaultTimeout {
		t.Errorf("0 应回落 DefaultTimeout(%v)，实际=%v", DefaultTimeout, b.idle)
	}
}

// TestMonitorNilBody —— nil body 应安全返回 nil。
func TestMonitorNilBody(t *testing.T) {
	if got := Monitor(nil, nil, time.Second); got != nil {
		t.Error("nil body 应返回 nil")
	}
}

// TestWithCancel —— WithCancel 应把 cancel 挂到请求 context 上。
func TestWithCancel(t *testing.T) {
	req, err := http.NewRequest("POST", "http://example.invalid/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req2, cancel := WithCancel(req)
	defer cancel()
	if req2 == req {
		t.Error("WithCancel 应返回新请求")
	}
	if req2.Context() == req.Context() {
		t.Error("新请求应有独立 context")
	}
	cancel()
	if req2.Context().Err() == nil {
		t.Error("cancel 后 context 应已取消")
	}
}

// TestIdleThroughRealServer —— 端到端：真实 httptest 服务端吐一段数据后卡死，
// 验证客户端读会在 idle 阈值内结束。
func TestIdleThroughRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: hello\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 卡死：等客户端断开
		<-r.Context().Done()
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	req, cancel := WithCancel(req)
	defer cancel()

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body := Monitor(cancel, resp.Body, 200*time.Millisecond)
	defer body.Close()

	buf := make([]byte, 64)
	if n, err := body.Read(buf); err != nil || n == 0 {
		t.Fatalf("应读到首段数据，n=%d err=%v", n, err)
	}

	// 卡死后应在阈值内返回错误
	done := make(chan error, 1)
	go func() { _, e := body.Read(buf); done <- e }()
	select {
	case e := <-done:
		if e == nil {
			t.Error("卡死后应返回错误")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("端到端：卡死流未在阈值内断流")
	}
}

// TestTickClamped —— tick 应钳在 [10ms, 1s]。
func TestTickClamped(t *testing.T) {
	if d := tick(time.Millisecond); d < 10*time.Millisecond {
		t.Errorf("极小 idle 的 tick 应 >=10ms，实际=%v", d)
	}
	if d := tick(time.Hour); d > time.Second {
		t.Errorf("极大 idle 的 tick 应 <=1s，实际=%v", d)
	}
	if d := tick(400 * time.Millisecond); d != 100*time.Millisecond {
		t.Errorf("tick 应为 idle/4，实际=%v", d)
	}
}
