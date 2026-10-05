package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 原生续接的端到端回归：会话 ID 经 Server Action 登记，续接轮次只发增量
// （见 internal/facade/native.go）。

func startConv(t *testing.T, up *fakeUpstream, n int) string {
	t.Helper()
	up.mu.Lock()
	defer up.mu.Unlock()
	cid, _ := up.startBodies[n]["conversationId"].(string)
	return cid
}

// Codex 桥：每轮带完整历史；首轮登记会话发全量，之后只发新增条目，system 不重复。
func TestE2E_Native_CodexSendsOnlyIncrements(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-native-1"}
	base := []any{
		codexMsg("developer", "<permissions instructions>workspace-write</permissions instructions>"),
		codexMsg("user", "记住本任务的暗号：COBALT-5521。读取 a.txt"),
	}
	post := func(items ...any) {
		t.Helper()
		if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", items...), hdr); code != http.StatusOK {
			t.Fatalf("状态码 %d: %.300s", code, out)
		}
	}

	post(base...)
	cid := startConv(t, up, 0)
	if !strings.HasPrefix(cid, "cdx1_") {
		t.Fatalf("首轮应带登记过的会话 ID，得到 %q", cid)
	}
	sys, user := requireSystemUser(t, upstreamInput(t, up, 0))
	if !strings.Contains(sys, "workspace-write") || !strings.Contains(user, "COBALT-5521") {
		t.Fatalf("首轮应发完整 system 与本轮消息: sys=%.120q user=%q", sys, user)
	}

	turn2 := append(append([]any{}, base...),
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": `{"command":["cat","a.txt"]}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "A-CONTENT-42"})
	post(turn2...)
	if got := startConv(t, up, 1); got != cid {
		t.Fatalf("第二轮应续接同一个会话: %q vs %q", got, cid)
	}
	sys, user = requireSystemUser(t, upstreamInput(t, up, 1))
	if strings.Contains(sys, "workspace-write") || strings.Contains(sys, "COBALT") || strings.Contains(user, "COBALT") {
		t.Fatalf("续接轮次不应重发 system 与往轮内容: sys=%.200q user=%.200q", sys, user)
	}
	if !strings.Contains(user, "A-CONTENT-42") {
		t.Fatalf("增量应是本轮工具结果: %q", user)
	}

	turn3 := append(append([]any{}, turn2...), codexMsg("assistant", "a.txt 的内容是 A-CONTENT-42"), codexMsg("user", "暗号是什么？"))
	post(turn3...)
	if _, user = requireSystemUser(t, upstreamInput(t, up, 2)); !strings.HasPrefix(user, "暗号是什么？") || strings.Contains(user, "A-CONTENT") || startConv(t, up, 2) != cid {
		t.Fatalf("第三轮应只发新提问并续接同一会话: %q", user)
	}
}

// Chat 客户端没有会话键（弱键）：连助手原文一起比对；对得上只发本轮，对不上新建会话发全量。
func TestE2E_Native_ChatWeakKey(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	reply := "你好，这是一段流式回答。（完）"
	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"记住暗号：菠萝油"}]}`)
	cid := startConv(t, up, 0)

	postLocal(t, ts.URL+"/v1/chat/completions", fmt.Sprintf(`{"model":"gpt-5","messages":[
		{"role":"user","content":"记住暗号：菠萝油"},{"role":"assistant","content":%q},{"role":"user","content":"暗号是什么？"}]}`, reply))
	_, user := requireSystemUser(t, upstreamInput(t, up, 1))
	if startConv(t, up, 1) != cid || user != "暗号是什么？" {
		t.Fatalf("同一段对话应续接并只发本轮: cid=%q user=%q", startConv(t, up, 1), user)
	}

	// 另一段对话：同样的开头，但助手回复不同 —— 不能串进上一个会话。
	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[
		{"role":"user","content":"记住暗号：菠萝油"},{"role":"assistant","content":"别的回答"},{"role":"user","content":"暗号是什么？"}]}`)
	sys, _ := requireSystemUser(t, upstreamInput(t, up, 2))
	if got := startConv(t, up, 2); got == cid || got == "" || !strings.Contains(sys, "菠萝油") {
		t.Fatalf("对不上时应新建会话并发全量历史: cid=%q sys=%.200q", got, sys)
	}
}

// 续接的会话在上游已不可用：作废绑定，换新会话、用客户端带来的完整历史重来，客户端无感。
func TestE2E_Native_GoneConversationRetriesFresh(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "native-gone"}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"记住暗号：GONE-7"}]}`, hdr); code != http.StatusOK {
		t.Fatalf("首轮失败 %d: %.200s", code, out)
	}
	cid := startConv(t, up, 0)
	up.mu.Lock()
	up.goneConvs = map[string]bool{cid: true}
	up.mu.Unlock()

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[
		{"role":"user","content":"记住暗号：GONE-7"},{"role":"assistant","content":"好"},{"role":"user","content":"暗号是什么？"}]}`, hdr)
	if code != http.StatusOK || !strings.Contains(out, "流式回答") {
		t.Fatalf("应换新会话重试成功 %d: %.300s", code, out)
	}
	n := startCount(up)
	if n != 3 || startConv(t, up, 1) != cid {
		t.Fatalf("应先续接旧会话、失败后重试一次: 共 %d 次 start", n)
	}
	if _, user := requireSystemUser(t, upstreamInput(t, up, 1)); user != "暗号是什么？" {
		t.Fatalf("续接时应只发本轮: %q", user)
	}
	fresh := startConv(t, up, 2)
	sys, _ := requireSystemUser(t, upstreamInput(t, up, 2))
	if fresh == cid || fresh == "" || !strings.Contains(sys, "GONE-7") {
		t.Fatalf("重试应用新会话并带上客户端的完整历史: cid=%q sys=%.200q", fresh, sys)
	}
}

// 登记会话失败：退回无状态全量（不带会话 ID），请求照常成功。
func TestE2E_Native_RegistrationFailureFallsBack(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t, actionFails: true}, goodAccount(), nil)
	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`)
	if cid := startConv(t, up, 0); cid != "" {
		t.Fatalf("登记失败时不应带会话 ID，得到 %q", cid)
	}
}

// Responses 客户端凭 previous_response_id 续接、只发本轮：找回同一个上游会话直接追加，
// 不重新登记会话；回复句柄不透传给上游（上游只认登记过的会话 ID）。
func TestE2E_Native_PreviousResponseIDContinues(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", `{"model":"gpt-5","stream":false,"input":"记住暗号：PREV-31"}`,
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("首轮失败 %d: %.200s", code, out)
	}
	var first struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &first); err != nil || first.ID == "" {
		t.Fatalf("首轮应返回 response id: %v %.200s", err, out)
	}
	cid := startConv(t, up, 0)

	body := fmt.Sprintf(`{"model":"gpt-5","stream":false,"input":"暗号是什么？","previous_response_id":%q}`, first.ID)
	if code, out = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body, map[string]string{"Content-Type": "application/json"}); code != http.StatusOK {
		t.Fatalf("续接轮失败 %d: %.200s", code, out)
	}
	_, user := requireSystemUser(t, upstreamInput(t, up, 1))
	up.mu.Lock()
	prev, convs := up.startBodies[1]["previousResponseId"], up.convSeq
	up.mu.Unlock()
	if startConv(t, up, 1) != cid || user != "暗号是什么？" || convs != 1 {
		t.Fatalf("应续接同一个会话、只发本轮、不重新登记: cid=%q user=%q convs=%d", startConv(t, up, 1), user, convs)
	}
	if prev != nil {
		t.Fatalf("回复句柄不应透传给上游: %v", prev)
	}
}

// 网关没有绑定（如重启后）而 Codex 带来的历史一条放不下：新建会话后先分段补种完整历史，
// 再发本轮 —— 上游拿到全部历史，而不是裁剪后的尾巴。
func TestE2E_Native_SeedsLongHistoryIntoNewConversation(t *testing.T) {
	const limit = 24000
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxPromptBytes = limit })
	items := []any{codexMsg("user", "记住本任务的暗号：COBALT-5521")}
	for k := 1; k <= 12; k++ {
		items = append(items,
			map[string]any{"type": "function_call", "call_id": fmt.Sprintf("c%d", k), "name": "shell", "arguments": fmt.Sprintf(`{"command":["cat","d%02d.txt"]}`, k)},
			map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("c%d", k), "output": fmt.Sprintf("OUT-%02d %s", k, strings.Repeat("o", 3000))})
	}
	items = append(items, codexMsg("user", "暗号是什么？"))
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "native-seed"}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", items...), hdr); code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}

	n := startCount(up)
	if n < 3 {
		t.Fatalf("应先补种若干段再发本轮，共 %d 次 start", n)
	}
	cid := startConv(t, up, 0)
	var seeded strings.Builder
	for i := 0; i < n-1; i++ {
		_, user := requireSystemUser(t, upstreamInput(t, up, i))
		if startConv(t, up, i) != cid || !strings.HasPrefix(user, fmt.Sprintf("[Earlier conversation, part %d of %d", i+1, n-1)) {
			t.Fatalf("第 %d 次 start 应是同一会话的补种段: %.80q", i+1, user)
		}
		if len(user) > limit {
			t.Fatalf("补种段超过上限: %d", len(user))
		}
		seeded.WriteString(user)
	}
	for _, want := range []string{"COBALT-5521", "OUT-01 ", "OUT-12 "} {
		if !strings.Contains(seeded.String(), want) {
			t.Errorf("补种内容缺少 %q（不该被裁掉）", want)
		}
	}
	sys, user := requireSystemUser(t, upstreamInput(t, up, n-1))
	if startConv(t, up, n-1) != cid || !strings.HasPrefix(user, "暗号是什么？") || strings.Contains(sys, "OUT-0") {
		t.Fatalf("最后一轮应在同一会话里只发本轮: sys=%.120q user=%.80q", sys, user)
	}
}

// Codex 本地压缩：压缩请求在上游会话里直接让上游写摘要；压缩后的替换历史认出摘要，
// 继续用同一个上游会话（上游那边保留着压缩前的完整记忆）。
func TestE2E_Native_CompactionKeepsConversation(t *testing.T) {
	summary := "## Progress\n- 暗号 COBALT-5521；a.txt 已读，内容 A-CONTENT-42。下一步：写 out.txt。"
	up := &fakeUpstream{t: t, replyParts: []string{summary}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "native-compact"}
	post := func(kind string, items ...any) {
		t.Helper()
		if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, kind, items...), hdr); code != http.StatusOK {
			t.Fatalf("状态码 %d: %.300s", code, out)
		}
	}
	task := codexMsg("user", "记住暗号 COBALT-5521，读取 a.txt")
	post("turn", task)
	cid := startConv(t, up, 0)
	hist := []any{task,
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": `{"command":["cat","a.txt"]}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "A-CONTENT-42"}}
	post("compaction", append(append([]any{}, hist...), codexMsg("user", "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary."))...)
	if sys, user := requireSystemUser(t, upstreamInput(t, up, 1)); startConv(t, up, 1) != cid ||
		!strings.Contains(user, "CONTEXT CHECKPOINT") || !strings.Contains(user, "A-CONTENT-42") ||
		strings.Contains(user, "COBALT") || !strings.Contains(sys, "<context_checkpoint>") {
		t.Fatalf("压缩请求应在同一会话里只发上游没见过的条目（工具结果 + 压缩指令），附压缩约束: user=%.200q", user)
	}

	post("turn", task, codexMsg("user", summaryPrefix+"\n"+summary), codexMsg("user", "继续：写 out.txt"))
	_, user := requireSystemUser(t, upstreamInput(t, up, 2))
	if startConv(t, up, 2) != cid || !strings.HasPrefix(user, "继续：写 out.txt") {
		t.Fatalf("压缩后应接着用同一个会话、只发新消息: cid=%q user=%.120q", startConv(t, up, 2), user)
	}
}

// 同一会话上一轮还没结束就来了新请求（用户打断后接着说、断线重发）：停掉上一轮，
// 在原上游会话里接着发增量，不另起会话整份重发；被停掉那一轮已送达的内容不再重发。
func TestE2E_Native_NewRequestSupersedesRunningTurn(t *testing.T) {
	up := &fakeUpstream{t: t, stallFirst: 100000}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-supersede-1"}
	turn1 := []any{
		codexMsg("developer", "<permissions instructions>workspace-write</permissions instructions>"),
		codexMsg("user", "第一问：FIRST-QUESTION"),
	}
	done := postAsync(ts.URL+"/v1/responses", codexBody(t, "turn", turn1...), hdr)
	waitFor(t, func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.statusBodies) > 0
	})

	turn2 := append(append([]any{}, turn1...),
		codexMsg("developer", "<turn_aborted>\nThe previous turn was interrupted on purpose.\n</turn_aborted>"),
		codexMsg("user", "第二问：SECOND-QUESTION"))
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", turn2...), hdr); code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("上一轮应被停掉")
	}
	up.mu.Lock()
	stops := len(up.stopBodies)
	up.mu.Unlock()
	if stops == 0 {
		t.Fatal("应通知上游停止上一轮")
	}
	if a, b := startConv(t, up, 0), startConv(t, up, 1); a == "" || a != b {
		t.Fatalf("第二轮应续接同一个上游会话: %q vs %q", a, b)
	}
	sys, user := requireSystemUser(t, upstreamInput(t, up, 1))
	if strings.Contains(sys, "workspace-write") || strings.Contains(user, "FIRST-QUESTION") ||
		!strings.Contains(user, "SECOND-QUESTION") || !strings.Contains(user, "interrupted") {
		t.Fatalf("第二轮应只发打断说明与新消息: sys=%.120q user=%q", sys, user)
	}
}

// 同一轮原样重发（断线重连）：停掉上一次，在原会话里只重发本轮消息。
func TestE2E_Native_IdenticalRetryResendsOnlyCurrent(t *testing.T) {
	up := &fakeUpstream{t: t, stallFirst: 100000}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-supersede-2"}
	turn := []any{
		codexMsg("developer", "<permissions instructions>workspace-write</permissions instructions>"),
		codexMsg("user", "只有一问：ONLY-QUESTION"),
	}
	postAsync(ts.URL+"/v1/responses", codexBody(t, "turn", turn...), hdr)
	waitFor(t, func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.statusBodies) > 0
	})
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", turn...), hdr); code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}
	if a, b := startConv(t, up, 0), startConv(t, up, 1); a == "" || a != b {
		t.Fatalf("重发应续接同一个上游会话: %q vs %q", a, b)
	}
	sys, user := requireSystemUser(t, upstreamInput(t, up, 1))
	if strings.Contains(sys, "workspace-write") || !strings.HasPrefix(user, "只有一问：ONLY-QUESTION") {
		t.Fatalf("重发应只带本轮消息: sys=%.120q user=%q", sys, user)
	}
}

// waitFor 轮询等条件成立（最多 10 秒）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// postAsync 在后台发请求（不碰 *testing.T），请求结束时关闭返回的通道。
func postAsync(url, body string, hdr map[string]string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			return
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	return done
}

// 续接轮次的用量：输入按整段会话计，其中上游会话里已有、本轮没重发的部分报为缓存命中；
// 新建会话那一轮全是新发的，缓存为 0。
func TestE2E_Native_ReportsCachedInput(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-cached-1"}
	usage := func(out string) (in, cached int) {
		t.Helper()
		mi := regexp.MustCompile(`"input_tokens":(\d+)`).FindAllStringSubmatch(out, -1)
		mc := regexp.MustCompile(`"input_tokens_details":\{"cached_tokens":(\d+)\}`).FindAllStringSubmatch(out, -1)
		if len(mi) == 0 || len(mc) == 0 {
			t.Fatalf("响应里没有用量: %.300s", out)
		}
		in, _ = strconv.Atoi(mi[len(mi)-1][1])
		cached, _ = strconv.Atoi(mc[len(mc)-1][1])
		return in, cached
	}
	turn1 := []any{
		codexMsg("developer", "<permissions instructions>"+strings.Repeat("规则 ", 3000)+"</permissions instructions>"),
		codexMsg("user", "第一问"),
	}
	_, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", turn1...), hdr)
	if in, cached := usage(out); cached != 0 || in < 3000 {
		t.Fatalf("新建会话那一轮不该有缓存命中: in=%d cached=%d", in, cached)
	}
	turn2 := append(append([]any{}, turn1...), codexMsg("assistant", "好"), codexMsg("user", "第二问"))
	_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", codexBody(t, "turn", turn2...), hdr)
	in, cached := usage(out)
	if cached < 3000 || cached >= in || in-cached > 2000 {
		t.Fatalf("续接轮次应把已在上游会话里的部分报为缓存命中: in=%d cached=%d", in, cached)
	}
}
