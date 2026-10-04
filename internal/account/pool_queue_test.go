package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func queuePool(t *testing.T, wait time.Duration, accounts ...config.AccountConfig) *Pool {
	t.Helper()
	p := testPool(t, "least_inflight", accounts...)
	p.cfg.QueueWait = wait
	return p
}

// 全部满并发时排队等空位，空出来就拿到，而不是立刻报错。
func TestPool_QueueWaitsForSlot(t *testing.T) {
	p := queuePool(t, 5*time.Second,
		config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1},
		config.AccountConfig{ID: "b", AccessToken: "t2", MaxConcurrency: 1},
	)
	l1, _ := p.Acquire(context.Background(), "")
	l2, _ := p.Acquire(context.Background(), "")
	go func() {
		time.Sleep(150 * time.Millisecond)
		l2.Release()
	}()
	start := time.Now()
	l3, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("应排队拿到空位：%v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("应等到空位释放")
	}
	l1.Release()
	l3.Release()
}

// 先来先到：先排队的先拿到，后来的不能插队抢刚空出来的位置。
func TestPool_QueueIsFIFO(t *testing.T) {
	p := queuePool(t, 5*time.Second, config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1})
	hold, _ := p.Acquire(context.Background(), "")

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second", "third"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			l, err := p.Acquire(context.Background(), "")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			l.Release()
		}(name)
		time.Sleep(30 * time.Millisecond) // 保证入队顺序
	}
	if p.QueueLen() != 3 {
		t.Fatalf("排队数 = %d，应为 3", p.QueueLen())
	}
	hold.Release()
	wg.Wait()
	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("出队顺序 = %v", order)
	}
}

// 排队到上限仍无空位：报容量不足（ErrPoolBusy），不是凭据问题。
func TestPool_QueueTimeout(t *testing.T) {
	p := queuePool(t, 120*time.Millisecond, config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1})
	hold, _ := p.Acquire(context.Background(), "")
	defer hold.Release()
	start := time.Now()
	_, err := p.Acquire(context.Background(), "")
	if !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("err = %v，应为 ErrPoolBusy", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("应排满 queue_wait 再报错")
	}
	if p.QueueLen() != 0 {
		t.Fatal("超时后应离开队列")
	}
}

// queue_wait 为 0 时保持旧行为：立即报错。
func TestPool_QueueDisabled(t *testing.T) {
	p := queuePool(t, 0, config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1})
	hold, _ := p.Acquire(context.Background(), "")
	defer hold.Release()
	start := time.Now()
	_, err := p.Acquire(context.Background(), "")
	if !errors.Is(err, ErrPoolBusy) || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("应立即报 ErrPoolBusy：%v（%s）", err, time.Since(start))
	}
}

// 客户端断开时离开队列。
func TestPool_QueueHonorsCancel(t *testing.T) {
	p := queuePool(t, 5*time.Second, config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1})
	hold, _ := p.Acquire(context.Background(), "")
	defer hold.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if p.QueueLen() != 0 {
		t.Fatal("断开后应离开队列")
	}
}

// 空闲时依次开的会话要分摊到两个号上，而不是全落在排第一的号。
func TestPool_NewSessionsSpreadAcrossAccounts(t *testing.T) {
	p := queuePool(t, time.Second,
		config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 3},
		config.AccountConfig{ID: "b", AccessToken: "t2", MaxConcurrency: 3},
	)
	count := map[string]int{}
	for _, s := range []string{"s1", "s2", "s3", "s4"} {
		l, err := p.Acquire(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		count[l.Account.ID]++
		l.Release() // 会话之间是空闲的：只比在途数时四个都会落在 a
	}
	if count["a"] != 2 || count["b"] != 2 {
		t.Fatalf("会话分布 = %v，应为各 2", count)
	}
}

// 网关重启后粘性表为空：Prefer 按落盘的绑定把会话认回原来的号。
func TestPool_PreferRestoresBinding(t *testing.T) {
	p := queuePool(t, time.Second,
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)
	p.Prefer("sess", "b")
	l, err := p.Acquire(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if l.Account.ID != "b" {
		t.Fatalf("应认回绑定的号 b，实际 %s", l.Account.ID)
	}
	// 已有粘性记录时不覆盖。
	p.Prefer("sess", "a")
	l, _ = p.Acquire(context.Background(), "sess")
	l.Release()
	if l.Account.ID != "b" {
		t.Fatalf("Prefer 不应覆盖已有绑定，实际 %s", l.Account.ID)
	}
	// 不存在的号忽略。
	p.Prefer("other", "zzz")
	if _, ok := p.sticky.Get("other", time.Now()); ok {
		t.Fatal("不存在的号不应写入粘性表")
	}
}
