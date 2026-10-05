package sentinel

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSign 每次签名耗时 d，签出 tok-1、tok-2 …
func fakeSign(d time.Duration, n *atomic.Int64) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		time.Sleep(d)
		return fmt.Sprintf("tok-%d", n.Add(1)), nil
	}
}

// 后台先签好：第二次来时直接拿现成的，不再等签名。
func TestPresigner_ServesReadyTokens(t *testing.T) {
	var n atomic.Int64
	p := newPresigner(fakeSign(50*time.Millisecond, &n), 2, time.Minute)
	defer p.close()
	if _, err := p.take(context.Background()); err != nil { // 第一次：要等签好
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 后台补满
	t0 := time.Now()
	tok, err := p.take(context.Background())
	if err != nil || tok == "" {
		t.Fatal(tok, err)
	}
	if d := time.Since(t0); d > 20*time.Millisecond {
		t.Fatalf("应直接拿到现成的，实际等了 %s", d)
	}
	if p.hits.Load() < 1 {
		t.Fatal("应记一次命中")
	}
}

// 闲着时攒满就停：不在后台持续签。
func TestPresigner_IdleStopsAtCapacity(t *testing.T) {
	var n atomic.Int64
	p := newPresigner(fakeSign(5*time.Millisecond, &n), 2, time.Minute)
	defer p.close()
	if _, err := p.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	// 拿走 1 个，补 1 个；初始 2 个空位 —— 总共最多签 3 个。
	if got := n.Load(); got > 3 {
		t.Fatalf("闲着时不应持续签名，已签 %d 个", got)
	}
}

// 放太久的丢弃，换新签的。
func TestPresigner_DropsStale(t *testing.T) {
	var n atomic.Int64
	p := newPresigner(fakeSign(5*time.Millisecond, &n), 1, 50*time.Millisecond)
	defer p.close()
	if _, err := p.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 补签的那个放过期了
	tok, err := p.take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.stale.Load() != 1 || tok != "tok-3" {
		t.Fatalf("应丢弃过期的 tok-2、给新签的 tok-3：stale=%d tok=%s", p.stale.Load(), tok)
	}
}

// 签名出错时把错误交给调用方（传输层据此发占位令牌），后台继续工作。
func TestPresigner_PropagatesError(t *testing.T) {
	var calls atomic.Int64
	boom := errors.New("boom")
	p := newPresigner(func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return "", boom
		}
		return "ok", nil
	}, 1, time.Minute)
	defer p.close()
	if _, err := p.take(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if tok, err := p.take(ctx); err != nil || tok != "ok" {
		t.Fatalf("出错后应恢复: %q %v", tok, err)
	}
}

// 调用方断开时不再等。
func TestPresigner_HonorsCancel(t *testing.T) {
	p := newPresigner(func(ctx context.Context) (string, error) {
		time.Sleep(time.Second)
		return "late", nil
	}, 1, time.Minute)
	defer p.close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
