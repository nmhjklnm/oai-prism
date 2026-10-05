package facade

import (
	"context"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
)

// 项目预热池：把"新会话首轮要付的建项目 + 沙箱同步"搬到空闲时提前做。
//
// 背景（2026-10-04 实测）：新会话首轮（建项目 + 申请沙箱 + 同步 + 生成）约
// 14 秒，其中建项目与同步占近半；同会话后续轮只要 5-6 秒。预热池为每个账号
// 常备 N 个"已建好且已同步"的项目，新会话来了直接领走（warmTake），后台
// 循环再补（warmLoop）。
//
// 与 lvzhentao/chatgpt-prism2api 的 PrewarmAccounts 相邻但不同：他们预热
// 的是「账号的沙箱保活」（多号 TTFB 择优），这里预热的是「项目」——
// 项目按会话创建、不可跨会话复用，只有提前建好等新会话来领才有意义。

// warmEntryTTL 是预热条目里"沙箱同步"的保鲜期。资源令牌实测 1 小时有效、沙箱闲置约
// 25 分钟被回收：取 40 分钟。过期的只是同步状态，项目本身一直可用 —— 条目保留、标为
// 未同步，领走它的会话在正常路径里重做同步。
//
// 以前过期就丢弃重建：空闲时每 40 分钟每个号新建 2 个项目，用不上的就扔在账号里。
// 2026-10-05 实测本机网关 6 小时建了 39 个、只领走 3 个；上游没有删项目的接口可用，
// 扔掉的项目永久留在账号的项目列表里。
const warmEntryTTL = 40 * time.Minute

// warmCreateGap 是同账号两次预热创建之间的间隔：预热不该跟 Sentinel 风控
// 硬碰，慢一点无所谓（这本来就是空闲时干的活）。
const warmCreateGap = 15 * time.Second

type warmEntry struct {
	projectID string
	synced    bool
	warmedAt  time.Time
}

type warmPool struct {
	mu        sync.Mutex
	byAccount map[string][]warmEntry
	size      int
	refill    chan struct{}
}

func newWarmPool(size int) *warmPool {
	return &warmPool{
		byAccount: make(map[string][]warmEntry, 4),
		size:      size,
		refill:    make(chan struct{}, 1),
	}
}

// warmTopUp 把每个可用账号的预热池补到容量。
// 逐账号串行、逐个创建：一次失败就跳过该账号本轮（下个周期再试），
// 不让预热风暴放大上游的风控抖动。
func (r *Runner) warmTopUp(ctx context.Context) {
	now := time.Now()
	for _, acct := range r.pool.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if !acct.Available(now) {
			continue
		}
		r.warm.gcAccount(acct.ID, now)
		r.warm.mu.Lock()
		have := len(r.warm.byAccount[acct.ID])
		r.warm.mu.Unlock()
		for ; have < r.warm.size; have++ {
			if ctx.Err() != nil {
				return
			}
			id, synced := r.warmCreate(ctx, acct)
			if id == "" {
				break
			}
			r.warm.mu.Lock()
			r.warm.byAccount[acct.ID] = append(r.warm.byAccount[acct.ID], warmEntry{
				projectID: id, synced: synced, warmedAt: time.Now(),
			})
			r.warm.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(warmCreateGap):
			}
		}
	}
}

// warmCreate 建一个项目并（开了沙箱时）把工作区同步做完。
// 项目建成就有价值（省掉 POST /api/projects）；沙箱同步失败也保留条目，
// 领走它的会话会在正常路径里重做同步。
func (r *Runner) warmCreate(ctx context.Context, acct *account.Account) (string, bool) {
	id, err := r.createProject(ctx, acct)
	if err != nil {
		r.log.Warn("预热：创建项目失败（本轮跳过该账号）", "account", acct.ID, "err", err)
		r.app.ProjectOps.Inc("warm", "create_error")
		return "", false
	}
	r.app.ProjectOps.Inc("warm", "create")
	if !r.cfg.Facade.UseSandbox {
		return id, false
	}
	sb, err := r.ensureSandbox(ctx, acct, id)
	if err != nil || !sb.Usable() {
		return id, false
	}
	_, outcome := r.syncOrRebuild(ctx, acct, sb, id)
	return id, outcome == syncReady
}

// warmTake 领走一个预热项目（优先最新、同步完成的）。
// 领走后非阻塞地踢一脚补充信号。池未启用时安全返回 false。
func (r *Runner) warmTake(accountID string) (string, bool) {
	if r.warm == nil {
		return "", false
	}
	now := time.Now()
	r.warm.mu.Lock()
	defer r.warm.mu.Unlock()
	entries := r.warm.gcAccountLocked(accountID, now)
	if len(entries) == 0 {
		return "", false
	}
	// 从尾部优先挑 synced 的；没有 synced 的就用最新的（同步在正常路径补）。
	pick := len(entries) - 1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].synced {
			pick = i
			break
		}
	}
	id := entries[pick].projectID
	entries = append(entries[:pick], entries[pick+1:]...)
	r.warm.byAccount[accountID] = entries
	select {
	case r.warm.refill <- struct{}{}:
	default:
	}
	r.app.ProjectOps.Inc("warm", "handout")
	return id, true
}

// warmSize 供指标/诊断使用。
func (r *Runner) warmSize() int {
	if r.warm == nil {
		return 0
	}
	r.warm.mu.Lock()
	defer r.warm.mu.Unlock()
	n := 0
	for _, es := range r.warm.byAccount {
		n += len(es)
	}
	return n
}

func (w *warmPool) gcAccount(accountID string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gcAccountLocked(accountID, now)
}

// gcAccountLocked 把同步已过期的条目标为未同步（项目保留，见 warmEntryTTL）。
func (w *warmPool) gcAccountLocked(accountID string, now time.Time) []warmEntry {
	es := w.byAccount[accountID]
	for i := range es {
		if es[i].synced && now.Sub(es[i].warmedAt) >= warmEntryTTL {
			es[i].synced = false
		}
	}
	return es
}

// warmLoop 是预热池的后台循环：周期巡检 + 领走后的补充信号。
func (r *Runner) warmLoop(ctx context.Context) {
	interval := 30 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.warm.refill:
		}
		r.warmTopUp(ctx)
	}
}
