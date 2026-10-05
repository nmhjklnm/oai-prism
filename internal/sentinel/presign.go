package sentinel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// 提前签好备用的 token。
//
// 发往 Prism 的请求（发起一轮、每次问"好了没"）都要先签一个一次性 token，现签
// 24 毫秒到 0.5 秒（慢的那次是 SDK 联网取 sentinel/req），而且串行 —— 这段时间直接
// 压在请求的等待里。presigner 用一个后台循环先签好最多 Presign 个，请求来了直接拿。
//
// 与 Prism 前端的差别只在"签"和"用"之间多隔了几秒：签名仍是同一个页面、同一个
// SDK、一次只签一个（后台循环是唯一的签名者）。约束：
//   - 用掉一个才补一个：闲着时攒满就停，不在后台持续签；
//   - 存放超过 PresignMaxAge 的丢弃不用（前端是签完立刻用，放久了可能被识破或过期）。
type presigner struct {
	sign   func(context.Context) (string, error) // 现签一个（Signer.signLocked）
	maxAge time.Duration
	ready  chan presigned // 签好待用
	room   chan struct{}  // 空位：拿走一个放回一个，循环据此补签
	stop   chan struct{}
	once   sync.Once

	hits  atomic.Int64 // 来了就有现成的
	stale atomic.Int64 // 放太久丢弃的
}

type presigned struct {
	tok string
	err error
	at  time.Time
}

func newPresigner(sign func(context.Context) (string, error), n int, maxAge time.Duration) *presigner {
	p := &presigner{
		sign: sign, maxAge: maxAge,
		ready: make(chan presigned, n),
		room:  make(chan struct{}, n),
		stop:  make(chan struct{}),
	}
	for range n {
		p.room <- struct{}{}
	}
	return p
}

// loop 是唯一的签名者：有空位就签一个放进 ready。
func (p *presigner) loop() {
	for {
		select {
		case <-p.room:
		case <-p.stop:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
		tok, err := p.sign(ctx)
		cancel()
		p.ready <- presigned{tok: tok, err: err, at: time.Now()}
		if err != nil {
			// 出错（页面重开、网络抖动）别紧接着再签，等一下。
			select {
			case <-time.After(time.Second):
			case <-p.stop:
				return
			}
		}
	}
}

// take 取一个可用的 token；没有现成的就等后台签好。
func (p *presigner) take(ctx context.Context) (string, error) {
	p.once.Do(func() { go p.loop() })
	first := true
	for {
		var it presigned
		select {
		case it = <-p.ready:
		default:
			first = false
			select {
			case it = <-p.ready:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		p.room <- struct{}{}
		if it.err != nil {
			return "", it.err
		}
		if time.Since(it.at) > p.maxAge {
			p.stale.Add(1)
			first = false
			continue
		}
		if first {
			p.hits.Add(1)
		}
		return it.tok, nil
	}
}

func (p *presigner) close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}
