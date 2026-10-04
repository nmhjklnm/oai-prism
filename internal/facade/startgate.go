package facade

import (
	"context"
	"sync"
	"time"
)

// 同号错开发起。
//
// 同一账号在同一瞬间发起多轮（response_with_tools_start），Prism 只放行一个，
// 其余回 "Error while processing conversation (403 Forbidden). Please submit prompt
// again."。2026-10-05 实测（同一账号、共用沙箱、3 个会话）：
//
//   - 每隔 3 秒发起一个（运行时间仍重叠）：0 次被拒，每轮 6.5~9.8 秒；
//   - 同一瞬间发起：被拒 2 次，被拒的两轮 17 秒、21 秒（原先被拒后要重同步沙箱工作区）；
//   - 6 个会话同一毫秒发起：拒 5 个，重发 8 次，最慢 64 秒。
//
// 所以拒绝的是"同号同时发起"，不是沙箱忙（错开后时间重叠也不拒）。
//
// 被拒之后不要急着原样重发：实测被拒后隔 2 秒、4 秒重发仍被拒，拒绝要持续好几秒，
// 早重发只是白等外加多发请求（那一轮反而拖到 34~36 秒）。被拒仍走 runOnce 原有的
// 沙箱重试路径（等 5 秒、重同步工作区再发），这里只负责别撞上。
//
// startGate 给每个账号排发起时刻：相邻两次至少隔 facade.start_gap，到点就发，
// 不等上一轮跑完。按预约排队，同时到的请求依次领到 0、gap、2·gap 的等待。
type startGate struct {
	mu   sync.Mutex
	next map[string]time.Time
}

func newStartGate() *startGate { return &startGate{next: map[string]time.Time{}} }

// reserve 为账号预约一个发起时刻，返回还要等多久（0 = 立即）。
func (g *startGate) reserve(accountID string, gap time.Duration, now time.Time) time.Duration {
	if g == nil || gap <= 0 {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at := g.next[accountID]
	if at.Before(now) {
		at = now
	}
	g.next[accountID] = at.Add(gap)
	return at.Sub(now)
}

// waitStartSlot 等到本账号的发起时刻。
func (r *Runner) waitStartSlot(ctx context.Context, accountID string) error {
	wait := r.starts.reserve(accountID, r.cfg.Facade.StartGap, time.Now())
	if wait <= 0 {
		return nil
	}
	r.log.Info("同号错开发起，等待", "account", accountID, "wait", wait.Round(time.Millisecond))
	return sleepCtx(ctx, wait)
}
