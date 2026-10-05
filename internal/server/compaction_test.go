package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

// Codex 压缩与上游单条上限的端到端回归（形状取自 2026-10-04 抓到的 Codex 0.160 真实请求）。

func codexMsg(role, text string) map[string]any {
	return map[string]any{"type": "message", "role": role,
		"content": []map[string]string{{"type": "input_text", "text": text}}}
}

// codexBody 组装一个桥请求体（带 additional_tools）；kind 非空时按 Codex 的方式标注请求种类。
func codexBody(t *testing.T, kind string, items ...any) string {
	t.Helper()
	input := append([]any{map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{}}}, items...)
	body := map[string]any{"model": "gpt-5", "stream": true, "input": input}
	if kind != "" {
		body["client_metadata"] = map[string]string{
			"x-codex-turn-metadata": fmt.Sprintf(`{"request_kind":%q,"compaction":{"implementation":"responses","strategy":"memento"}}`, kind),
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const summaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process."

func startCount(up *fakeUpstream) int {
	up.mu.Lock()
	defer up.mu.Unlock()
	return len(up.startBodies)
}

func promptLen(items []upstreamItem) int {
	n := 0
	for _, it := range items {
		n += len(it.text)
	}
	return n
}

// TestE2E_Bridge_PostCompactionKeepsSummary：压缩后的首个请求没有 assistant 条目。
// 旧判据不折叠，上游只读到本轮提问，摘要与保留的用户消息全丢；回落注入的会话链
// 也不该出现（2026-10-04 实测：空会话链重放时模型答"上下文未提供暗号"）。
func TestE2E_Bridge_PostCompactionKeepsSummary(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	session := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-thread-1"}

	// 同一会话先走一轮非桥请求，会话链里留下一段"陈旧历史"。
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses",
		`{"model":"gpt-5","input":"CHAIN-NOISE 请记住"}`, session); code != http.StatusOK {
		t.Fatalf("预热失败 %d: %.200s", code, out)
	}

	body := codexBody(t, "turn",
		codexMsg("developer", "You are Codex, an agent based on GPT-6. "+strings.Repeat("base ", 200)),
		codexMsg("user", "记住本任务的暗号：COBALT-5521"),
		codexMsg("user", "读取 data2.txt，告诉我 L037 的 MARKER"),
		codexMsg("user", summaryPrefix+"\n## Progress\n- data2.txt → MARKER-222"),
		codexMsg("developer", "<permissions instructions>workspace-write</permissions instructions>"),
		codexMsg("user", "<environment_context>\n  <cwd>C:\\proj</cwd>\n</environment_context>"),
		codexMsg("user", "暗号是什么？"),
	)
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body, session); code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}

	sys, user := requireSystemUser(t, upstreamInput(t, up, 1))
	for _, want := range []string{"COBALT-5521", "MARKER-222", summaryPrefix, "workspace-write"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system 缺少 %q", want)
		}
	}
	for _, bad := range []string{"CHAIN-NOISE", "You are Codex, an agent"} {
		if strings.Contains(sys, bad) {
			t.Errorf("system 不应包含 %q", bad)
		}
	}
	if !strings.HasPrefix(user, "暗号是什么？") {
		t.Fatalf("最后一条 user 应是本轮提问: %q", user)
	}
}

// TestE2E_Bridge_CompactionRequestFitsAndStaysText：登记会话失败、只能单条全量发送时，
// 压缩请求超过单条上限就裁掉最旧的历史而不是失败（这一轮必须成功，摘要才接得上）；
// 模型就算输出 codex-exec 块，回给 Codex 的也只能是摘要正文。
func TestE2E_Bridge_CompactionRequestFitsAndStaysText(t *testing.T) {
	const limit = 24000
	up := &fakeUpstream{t: t, actionFails: true, // 登记失败 → 单条全量回退（能登记时走补种，见 native_e2e_test）
		replyParts: []string{"## Progress\n- 暗号 COBALT-5521\n", "```codex-exec\nconst out = await tools.exec_command({ cmd: \"ls\" });\ntext(out);\n```"}}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxPromptBytes = limit })

	items := []any{codexMsg("user", "记住本任务的暗号：COBALT-5521")}
	for k := 1; k <= 12; k++ {
		items = append(items,
			map[string]any{"type": "custom_tool_call", "call_id": fmt.Sprintf("c%d", k), "name": "exec",
				"input": fmt.Sprintf(`const out = await tools.exec_command({ cmd: "type data%02d.txt" }); text(out);`, k)},
			map[string]any{"type": "custom_tool_call_output", "call_id": fmt.Sprintf("c%d", k),
				"output": fmt.Sprintf("OUT-%02d %s", k, strings.Repeat("o", 3000))})
	}
	items = append(items, codexMsg("user", "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary."))

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "compaction", items...),
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(out, "response.completed") {
		t.Fatalf("压缩请求应成功 %d: %.300s", code, out)
	}
	if strings.Contains(out, `"custom_tool_call"`) || strings.Contains(out, `"function_call"`) {
		t.Fatal("压缩请求的回复不能变成工具调用")
	}
	if !strings.Contains(out, "COBALT-5521") {
		t.Fatal("摘要正文应原样回给 Codex")
	}

	got := upstreamInput(t, up, 0)
	sys, _ := requireSystemUser(t, got)
	if n := promptLen(got); n > limit {
		t.Fatalf("发往上游 %d 字节，超过上限 %d", n, limit)
	}
	if !strings.Contains(sys, "omitted") || strings.Contains(sys, "OUT-01 ") || !strings.Contains(sys, "OUT-12 ") {
		t.Fatal("应裁掉最旧的工具输出、保留最新的，并注明省略")
	}
	if !strings.Contains(sys, "COBALT-5521") {
		t.Fatal("用户自己交代的暗号应保留：先丢的是工具输出")
	}
	if !strings.Contains(sys, "<context_checkpoint>") {
		t.Fatal("压缩请求应附带只回摘要的说明")
	}
}

// TestE2E_Bridge_OversizeHistoryTrimmed：登记会话失败、只能单条全量发送时，历史超过单条
// 上限就裁掉最旧的部分，而不是回 context_length_exceeded —— 实测 Codex 0.160 收到它只会
// 结束本轮，下一轮照发同样的历史，会话卡死。
func TestE2E_Bridge_OversizeHistoryTrimmed(t *testing.T) {
	const limit = 20000
	ts, up := newTestServer(t, &fakeUpstream{t: t, actionFails: true}, goodAccount(), func(c *config.Config) { c.Facade.MaxPromptBytes = limit })
	items := []any{codexMsg("user", "开始")}
	for k := 1; k <= 8; k++ {
		items = append(items,
			map[string]any{"type": "custom_tool_call", "call_id": fmt.Sprintf("c%d", k), "name": "exec", "input": "ls"},
			map[string]any{"type": "custom_tool_call_output", "call_id": fmt.Sprintf("c%d", k),
				"output": fmt.Sprintf("OUT-%02d %s", k, strings.Repeat("o", 3000))})
	}
	items = append(items, codexMsg("user", "继续"))

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", items...),
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK || !strings.Contains(out, "response.completed") {
		t.Fatalf("历史超限应裁剪后照常完成 (%d): %.400s", code, out)
	}
	got := upstreamInput(t, up, 0)
	sys, user := requireSystemUser(t, got)
	if n := promptLen(got); n > limit {
		t.Fatalf("发往上游 %d 字节，超过上限 %d", n, limit)
	}
	if !strings.Contains(sys, "omitted") || strings.Contains(sys, "OUT-01 ") || !strings.Contains(sys, "OUT-08 ") {
		t.Fatal("应裁掉最旧的工具输出、保留最新的，并注明省略")
	}
	if !strings.HasPrefix(user, "继续") {
		t.Fatalf("本轮提问不能被裁: %q", user)
	}
}

// TestE2E_Bridge_OversizeTurnSplit：本轮内容本身就超过单条上限时不报错，而是在同一个上游会话里
// 先把前几段补种进去、最后一段随本轮发出；每条都在上限内，内容一行不丢。
func TestE2E_Bridge_OversizeTurnSplit(t *testing.T) {
	const limit = 20000
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxPromptBytes = limit })
	var big strings.Builder
	for k := 1; k <= 30; k++ {
		fmt.Fprintf(&big, "LINE-%02d %s\n", k, strings.Repeat("长", 300))
	}
	big.WriteString("最后的问题：一共几行？")
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "split-turn"}
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", codexMsg("user", big.String())), hdr)
	if code != http.StatusOK || !strings.Contains(out, "response.completed") {
		t.Fatalf("超长的一轮应拆开后照常完成 (%d): %.400s", code, out)
	}
	n := startCount(up)
	if n < 2 {
		t.Fatalf("应先补种再发本轮，共 %d 次 start", n)
	}
	cid := startConv(t, up, 0)
	var all strings.Builder
	for i := 0; i < n; i++ {
		items := upstreamInput(t, up, i)
		_, user := requireSystemUser(t, items)
		if l := promptLen(items); l > limit {
			t.Fatalf("第 %d 次 start %d 字节，超过上限 %d", i+1, l, limit)
		}
		if startConv(t, up, i) != cid {
			t.Fatalf("第 %d 次 start 不在同一个上游会话里", i+1)
		}
		all.WriteString(user)
	}
	_, last := requireSystemUser(t, upstreamInput(t, up, n-1))
	if !strings.HasPrefix(last, "[The user's message for this turn, part ") || !strings.Contains(last, "最后的问题") {
		t.Fatalf("最后一次 start 应是本轮消息的最后一段: %.120q", last)
	}
	for k := 1; k <= 30; k++ {
		if !strings.Contains(all.String(), fmt.Sprintf("LINE-%02d ", k)) {
			t.Fatalf("拆分后丢了 LINE-%02d", k)
		}
	}
	// 用量要算上补种的段：上游实际读了全部 9000 个"长"，不能只按最后一段计。
	m := regexp.MustCompile(`"input_tokens":(\d+)`).FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		t.Fatal("response.completed 里没有 input_tokens")
	}
	if in, _ := strconv.Atoi(m[len(m)-1][1]); in < 9000 {
		t.Fatalf("输入用量 %d 没算上补种的段", in)
	}
}

// TestE2E_OversizeSplitAcrossAPIs：Chat、Anthropic、非桥 Responses 的超长一轮同样拆开补种，
// 不再在流开始前回绝。
func TestE2E_OversizeSplitAcrossAPIs(t *testing.T) {
	const limit = 20000
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxPromptBytes = limit })
	big := strings.Repeat("长 ", 10000)
	hdr := map[string]string{"Content-Type": "application/json"}
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"` + big + `"}]}`},
		{"/v1/messages", `{"model":"gpt-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"` + big + `"}]}`},
		{"/v1/responses", `{"model":"gpt-5","input":"` + big + `"}`},
	} {
		before := startCount(up)
		if code, out := doLocal(t, http.MethodPost, ts.URL+tc.path, tc.body, hdr); code != http.StatusOK {
			t.Fatalf("%s 超长的一轮应拆开后完成，得到 %d: %.300s", tc.path, code, out)
		}
		after := startCount(up)
		if after-before < 2 {
			t.Fatalf("%s 应先补种再发本轮，只发了 %d 次", tc.path, after-before)
		}
		for i := before; i < after; i++ {
			if l := promptLen(upstreamInput(t, up, i)); l > limit {
				t.Fatalf("%s 第 %d 次 start %d 字节，超过上限", tc.path, i-before+1, l)
			}
		}
	}
}

// TestE2E_UpstreamTooLargeMapsToInvalidPrompt：上游自己报单条超限（比如它收紧了限制）
// 要变成不可重试、也不动窗口的 invalid_prompt，而不是会被无限重试的 server_error，
// 更不是让 Codex 把窗口记满的 context_length_exceeded。
func TestE2E_UpstreamTooLargeMapsToInvalidPrompt(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json"}

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses",
		`{"model":"too-large-model","stream":true,"input":"你好"}`, hdr)
	if code != http.StatusOK || !strings.Contains(out, `"code":"invalid_prompt"`) || strings.Contains(out, "context_length_exceeded") {
		t.Fatalf("流式应以 invalid_prompt 失败 (%d): %.400s", code, out)
	}

	code, out = doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"too-large-model","messages":[{"role":"user","content":"你好"}]}`, hdr)
	if code != http.StatusBadRequest || !strings.Contains(out, `"code":"invalid_prompt"`) {
		t.Fatalf("chat 同步应 400 invalid_prompt，得到 %d: %.300s", code, out)
	}
}
