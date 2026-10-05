package facade

import (
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/tokens"
)

// 出字速度。
//
// 每轮总耗时里只有一小段是在出字：前面是建项目、申请沙箱、发起、排队、模型思考。
// 拿"输出 token / 总耗时"当速度会低估很多（实测 0.1~10 tok/s）。这里只量出字那一段：
// 第一段正文到达到最后一段正文到达之间多了多少 token。上游没有逐 token 推送，网关每
// 1~3 秒轮询一次拿累计正文，所以这是近似值：太短的回答（只到一两段）不算速度；
// 思考阶段的 token 上游不给，也不在内。
//
// 桥模式另记 block_tail_s：```codex-exec 块写完到上游宣布本轮结束隔了多久 —— 网关现在
// 要等整轮结束才取块（见 responses.go），这个数决定"块一写完就派发"值不值得做。

// outputRate 记录一轮里正文到达的时间线。
type outputRate struct {
	firstAt  time.Time // 第一段正文到达
	lastAt   time.Time // 最后一段正文到达
	firstTok int       // 第一段到达时累计正文的 token 数
	deltas   int       // 有新正文的轮询次数
	blockAt  time.Time // codex-exec 块闭合时（桥模式）
}

// observe 在某次轮询拿到新正文后调用；text 是到此为止的累计正文。
func (o *outputRate) observe(text string, bridge bool) {
	now := time.Now()
	if o.deltas == 0 {
		o.firstAt, o.firstTok = now, tokens.Count(text)
	}
	o.deltas++
	o.lastAt = now
	if bridge && o.blockAt.IsZero() && execBlockClosed(text) {
		o.blockAt = now
	}
}

// execBlockClosed 判断正文里的 ```codex-exec 块是否已经写完（出现了闭合围栏）。
func execBlockClosed(text string) bool {
	i := strings.Index(text, "```codex-exec")
	return i >= 0 && strings.Contains(text[i+len("```codex-exec"):], "```")
}

// log 在一轮成功结束时写一行"出字速度"。first_text_s 是发起到第一段正文（含排队、思考）。
func (o *outputRate) log(l *slog.Logger, model, account, text string, started time.Time) {
	now := time.Now()
	total := tokens.Count(text)
	attrs := []any{"model", model, "account", account, "out_tokens", total, "deltas", o.deltas,
		"total_s", secs(now.Sub(started))}
	if o.deltas > 0 {
		attrs = append(attrs, "first_text_s", secs(o.firstAt.Sub(started)))
	}
	if span := o.lastAt.Sub(o.firstAt); o.deltas >= 2 && span >= time.Second {
		gen := total - o.firstTok
		attrs = append(attrs, "gen_tokens", gen, "gen_span_s", secs(span),
			"tok_per_sec", math.Round(float64(gen)/span.Seconds()*10)/10)
	}
	if !o.blockAt.IsZero() {
		attrs = append(attrs, "block_tail_s", secs(now.Sub(o.blockAt)))
	}
	l.Info("出字速度", attrs...)
}

// secs 把时长写成保留两位小数的秒数（JSON 日志里 time.Duration 是纳秒整数，不好读也不好算）。
func secs(d time.Duration) float64 { return math.Round(d.Seconds()*100) / 100 }
