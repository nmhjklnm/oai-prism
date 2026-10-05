package facade

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestExecBlockClosed(t *testing.T) {
	cases := map[string]bool{
		"先看看。\n```codex-exec\nawait exec('ls')":       false,
		"先看看。\n```codex-exec\nawait exec('ls')\n```":  true,
		"```js\nx\n```\n还没有块":                         false,
		"```codex-exec\nawait exec('ls')\n```\n后面还有字": true,
	}
	for text, want := range cases {
		if got := execBlockClosed(text); got != want {
			t.Errorf("%q: got %v want %v", text, got, want)
		}
	}
}

// 出字段跨过 1 秒才算速度；桥模式记下块闭合后的尾巴。
func TestOutputRateLog(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	started := time.Now().Add(-5 * time.Second)

	var o outputRate
	o.observe("hello", true)
	o.firstAt = o.firstAt.Add(-2 * time.Second) // 假装第一段在 2 秒前到达
	text := "hello " + strings.Repeat("word ", 200) + "\n```codex-exec\nawait exec('ls')\n```"
	o.observeReasoning()
	o.observe(text, true)
	o.log(l, "m", "acct", text, started)
	line := buf.String()
	for _, k := range []string{"出字速度", "tok_per_sec=", "gen_tokens=", "gen_span_s=", "block_tail_s=", "first_text_s=", "deltas=2", "reasoning_parts=1", "last_reasoning_s=", "after_reasoning_s="} {
		if !strings.Contains(line, k) {
			t.Fatalf("日志缺 %s: %s", k, line)
		}
	}

	buf.Reset()
	var short outputRate
	short.observe("OK", false)
	short.log(l, "m", "acct", "OK", started)
	if line := buf.String(); strings.Contains(line, "tok_per_sec") || strings.Contains(line, "block_tail_s") || strings.Contains(line, "reasoning") {
		t.Fatalf("只有一段不该算速度、非桥不记块: %s", line)
	}
}
