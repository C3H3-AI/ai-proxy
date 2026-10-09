// Package idle 提供聊天 SSE 流的「流中空闲监控」：读到数据即续命，静默超阈值主动断流。
//
// 背景：本项目的流式专用 client（traework StreamHTTP / qoder 等）刻意不设总超时
// （http.Client.Timeout = 0），以支持长时间生成的回答。但这样就移除了总时长兜底 ——
// 若上游连上并在发出首字节后【中途卡死】不再发送数据，请求会无限期挂起，
// 连接与 goroutine 都无法释放。对多账号池而言，这会持续吃掉账号可用性
// （表现为「账号都在，但就是一直转、没有可用账号」）。
//
// 本包在返回给调用方的 body 外层包一层监控：每次读到底层数据就刷新时间戳，
// 后台按周期检查，静默超过 idle 即 cancel 请求 context，
// 使阻塞中的 Read 立即返回错误，从而释放连接与 goroutine。
//
// 首字节阶段不由本包负责：请求挂上 ctx 后，Transport 的 ResponseHeaderTimeout
// 仍会约束「响应头到达前」的时长；本包只从拿到 body 之后开始计时，不抢跑。
//
// 移植自上游 wild-work（internal/idle，PR #79）。
package idle

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeout 生产默认的流中空闲阈值：静默超过该时长即判定上游卡死并断流。
const DefaultTimeout = 300 * time.Second

// monitoringBody 包在流式 body 外层：每次读到底层数据（n>0）就刷新 lastRead；
// 后台 goroutine 周期检查，静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type monitoringBody struct {
	rc       io.ReadCloser
	cancel   context.CancelFunc
	idle     time.Duration
	mu       sync.Mutex
	lastRead time.Time
	closed   bool
	done     chan struct{}
}

// Read 透传读取；读到底层数据（n>0）时刷新 lastRead 续命。
func (b *monitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close 幂等：停掉后台监控 goroutine、取消请求 context、关闭底流，保证无泄漏。
func (b *monitoringBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.done)
	cancel := b.cancel
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return b.rc.Close()
}

// idleFor 返回距上次读到数据的静默时长。
func (b *monitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// Monitor 给流式请求挂上可取消的 context，并对其返回的 body 加空闲监控。
//
// 用法（req 必须来自 WithCancel）：
//
//	req, cancel := idle.WithCancel(req)
//	resp, err := client.Do(req)
//	if err != nil {
//	    cancel()
//	    return nil, err
//	}
//	defer cancel()
//	return resp.Body, idle.Monitor(cancel, resp.Body, idle.DefaultTimeout), nil
//
// 返回的 ReadCloser 同时负责：透传读、停止监控 goroutine、释放底层流。
func Monitor(cancel context.CancelFunc, rc io.ReadCloser, d time.Duration) io.ReadCloser {
	if d <= 0 {
		d = DefaultTimeout
	}
	if rc == nil {
		return nil
	}
	b := &monitoringBody{
		rc:       rc,
		cancel:   cancel,
		idle:     d,
		lastRead: time.Now(),
		done:     make(chan struct{}),
	}
	go func() {
		t := time.NewTicker(tick(d))
		defer t.Stop()
		for {
			select {
			case <-b.done:
				return
			case <-t.C:
				if b.idleFor() > b.idle {
					b.mu.Lock()
					already := b.closed
					b.mu.Unlock()
					if !already && cancel != nil {
						cancel()
					}
					return
				}
			}
		}
	}()
	return b
}

// WithCancel 给请求挂上可取消的 context，返回新请求与 cancel。
// cancel 应交给 Monitor 接管（见其注释）。
func WithCancel(req *http.Request) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithCancel(req.Context())
	return req.WithContext(ctx), cancel
}

// tick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大值避免空转。
func tick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
