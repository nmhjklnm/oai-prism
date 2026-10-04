package facade

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/oai-prism/oaiprism/internal/sse"
)

// web_search_call 合成展示。
//
// 上游的联网搜索发生在它的远程容器里，协议只回最终文本 —— 没有搜索事件
// 可转发，Codex CLI 因此画不出「Searched the web」的活动行。但答案正文里
// 带着真实的引用链接。为了界面完整，网关在答案前合成 web_search_call
// 输出条目：
//
//   - 1 条 search：查询词取本轮用户问题（推断值 —— 上游真用的词拿不到）；
//   - N 条 open_page：正文引用里提取的 URL（真实值，去重、上限 6 条）。
//
// 时序正确（搜索 → 打开来源 → 答案），但与答案同时到达：桥模式本来就是
// 整段返回，合成的不是流式过程。没有引用链接就不合成 —— 无中生有的
// 搜索行比没有更糟。
const synthesizedSearchMax = 6

var reMarkdownLink = regexp.MustCompile(`\[[^\]]*\]\((https?://[^)\s]+)\)`)

// synthesizedSearchItems 从答案正文提取引用，合成 web_search_call 条目 JSON。
// question 为空时跳过 search 条目（只列打开的来源）。
func synthesizedSearchItems(text, question string) []string {
	seen := map[string]bool{}
	var urls []string
	for _, m := range reMarkdownLink.FindAllStringSubmatch(text, -1) {
		u := m[1]
		if seen[u] {
			continue
		}
		seen[u] = true
		urls = append(urls, u)
		if len(urls) >= synthesizedSearchMax {
			break
		}
	}
	if len(urls) == 0 {
		return nil
	}
	var items []string
	if q := strings.TrimSpace(question); q != "" {
		items = append(items, webSearchCallItemJSON(map[string]any{
			"type": "search", "query": truncateRunesTo(q, 80),
		}))
	}
	for _, u := range urls {
		items = append(items, webSearchCallItemJSON(map[string]any{
			"type": "open_page", "url": u,
		}))
	}
	return items
}

func webSearchCallItemJSON(action map[string]any) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, newID("ws_"))
	sb.WriteString(`,"type":"web_search_call","status":"completed","action":`)
	b, err := json.Marshal(action)
	if err != nil {
		return ""
	}
	sb.Write(b)
	sb.WriteString(`}`)
	return sb.String()
}

// synthesizedSearchOutputs 是同步响应形态的同一批条目。
func synthesizedSearchOutputs(items []string) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		var m map[string]any
		if json.Unmarshal([]byte(it), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// lastUserQuestion 从 Codex 的完整请求历史里取最后一条"真人提问"。
//
// Codex 每轮重发全量历史，问题一定在 input 里；要跳过的三类条目：
// 静态注入（AGENTS.md / environment_context）、工具执行结果
// （[CLIENT RESULT] 开头）、以及非消息条目。拿到的是折叠前原文，
// 不受上游单条上限裁剪影响。
func lastUserQuestion(raw map[string]json.RawMessage) string {
	blocks, ok := rawJSONList(raw["input"])
	if !ok {
		return ""
	}
	best := ""
	for _, b := range blocks {
		var d struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		if (d.Type != "" && d.Type != "message") || !strings.EqualFold(d.Role, "user") {
			continue
		}
		text := strings.TrimSpace(contentPlainText(d.Content))
		if text == "" || isStaticInstruction(text) || strings.HasPrefix(text, "[CLIENT RESULT") {
			continue
		}
		best = text
	}
	best = strings.TrimSpace(strings.TrimSuffix(best, localExecReminder))
	return best
}

// writeReasoningItem 把一条思考摘要作为独立的 reasoning 条目流给客户端：
// output_item.added → summary_part.added → summary_text.delta → summary_text.done →
// summary_part.done → output_item.done。Codex 把它渲染成思考区块（与官方 Prism 页面的
// 灰色思考同源：上游 codex_live_progress.reasoningSummaries）。
//
// 每条摘要单独成一个条目：上游是一行一条地给，到一条发一条，不必等同一条目收尾。
func writeReasoningItem(sw *sse.Writer, text string) error {
	text = formatSummaryHeading(text)
	id := newID("rs_")
	var item strings.Builder
	item.WriteString(`{"id":`)
	writeJSONString(&item, id)
	item.WriteString(`,"type":"reasoning","summary":[]}`)
	var done strings.Builder
	done.WriteString(`{"id":`)
	writeJSONString(&done, id)
	done.WriteString(`,"type":"reasoning","summary":[{"type":"summary_text","text":`)
	writeJSONString(&done, text)
	done.WriteString(`}],"encrypted_content":null}`)

	var buf []byte
	for _, ev := range []ResponsesEvent{
		{Type: "response.output_item.added", ItemJSON: item.String()},
		{Type: "response.reasoning_summary_part.added", ItemID: id},
		{Type: "response.reasoning_summary_text.delta", ItemID: id, Text: text},
		{Type: "response.reasoning_summary_text.done", ItemID: id, Text: text},
		{Type: "response.reasoning_summary_part.done", ItemID: id, Text: text},
		{Type: "response.output_item.done", ItemJSON: done.String()},
	} {
		buf = AppendResponsesEvent(buf[:0], ev)
		if err := sw.WriteRaw(buf); err != nil {
			return err
		}
	}
	return nil
}

// formatSummaryHeading 把上游 "**标题** 正文" 的单行形态还原成 "**标题**\n\n正文"：
// Codex 以首行的 **…** 作为思考状态标题，正文另起段落。
func formatSummaryHeading(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "**") {
		return t
	}
	end := strings.Index(t[2:], "**")
	if end < 0 {
		return t
	}
	head, rest := t[:end+4], strings.TrimSpace(t[end+4:])
	if rest == "" {
		return head
	}
	return head + "\n\n" + rest
}
