package facade

import (
	"encoding/json"
	"strings"
	"testing"
)

// codexToolsRaw 构造一份带 MCP 工具的 Codex 请求（顶层 tools 路径）。
func codexToolsRaw() map[string]json.RawMessage {
	tools := `[` +
		`{"type":"function","name":"exec_command","description":"Runs a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},` +
		`{"type":"function","name":"mcp__ctx7__query_docs","description":"Query docs for a library.\n\nLong extra text.","parameters":{"type":"object","properties":{"libraryId":{"type":"string"},"query":{"type":"string"}},"required":["libraryId","query"]}},` +
		`{"type":"custom","name":"apply_patch","description":"Apply a patch"}` +
		`]`
	input := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`
	return map[string]json.RawMessage{
		"tools": json.RawMessage(tools),
		"input": json.RawMessage(input),
	}
}

func TestRegisteredClientToolsTopLevel(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	if len(tools) != 1 {
		t.Fatalf("应只登记 1 个非内建工具（mcp__ctx7__query_docs），得到 %d: %+v", len(tools), tools)
	}
	c := tools[0]
	if c.Name != "mcp__ctx7__query_docs" || c.Kind != "function" {
		t.Fatalf("工具识别错误: %+v", c)
	}
	if c.Args == "" || !strings.Contains(c.Args, "libraryId (required)") {
		t.Errorf("参数摘要应含 required 标记: %q", c.Args)
	}
	if !strings.Contains(c.Schema, `"libraryId"`) {
		t.Errorf("Schema 应保留原始 JSON: %q", c.Schema)
	}
}

// lite 路径：工具在 input 的 additional_tools 里，按 namespace 分组。
func TestRegisteredClientToolsNamespaced(t *testing.T) {
	input := `[
 {"type":"additional_tools","role":"developer","tools":[
  {"type":"namespace","name":"mcp__node_repl","tools":[
   {"type":"function","name":"js","description":"Run JS","parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}}
  ]},
  {"type":"function","name":"exec_command","parameters":{"type":"object"}}
 ]}
]`
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		t.Fatalf("测试输入不是合法 JSON: %v", err)
	}
	tools := registeredClientTools(map[string]json.RawMessage{"input": json.RawMessage(input)})
	if len(tools) != 1 || tools[0].Name != "mcp__node_repl__js" {
		t.Fatalf("namespace 组内工具应展开为全名: %+v", tools)
	}
}

func TestToolDocsSectionBudget(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	docs := toolDocsSection(tools)
	if !strings.Contains(docs, "mcp__ctx7__query_docs (function)") || !strings.Contains(docs, "JSON Schema:") {
		t.Errorf("目录应含完整条目: %s", docs)
	}
	if toolDocsSection(nil) != "" {
		t.Error("无工具时应返回空串")
	}
}

func TestParseBridgeCallsMCPPassthrough(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	js := "const r = await tools.mcp__ctx7__query_docs({ \"libraryId\": \"/openai/codex\", \"query\": \"shell tool\" });\ntext(r);"
	calls := parseBridgeCalls(js, tools, "exec_command", "function")
	if len(calls) != 1 {
		t.Fatalf("应解析出 1 个调用: %+v", calls)
	}
	if calls[0].Name != "mcp__ctx7__query_docs" || calls[0].Kind != "function" {
		t.Fatalf("调用识别错误: %+v", calls[0])
	}
	var args map[string]any
	if json.Unmarshal([]byte(calls[0].ArgsJSON), &args) != nil || args["libraryId"] != "/openai/codex" {
		t.Fatalf("参数应原样保留: %s", calls[0].ArgsJSON)
	}
}

// 纯 exec 块必须返回 nil（走带全部兜底加固的老路径）。
func TestParseBridgeCallsExecOnlyFallsBack(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	js := "const out = await tools.exec_command({ cmd: \"ls -la\" });\ntext(out);"
	if calls := parseBridgeCalls(js, tools, "exec_command", "function"); calls != nil {
		t.Fatalf("纯 exec 块应走老路径，得到 %+v", calls)
	}
}

// 混合块：exec 与 MCP 调用各成一条；未知工具名不透传。
func TestParseBridgeCallsMixed(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	js := "const a = await tools.exec_command({ cmd: \"pwd\" });\n" +
		"const r = await tools.mcp__ctx7__query_docs({ \"libraryId\": \"x\", \"query\": \"y\" });\n" +
		"const z = await tools.no_such_tool({ a: 1 });\ntext(r);"
	calls := parseBridgeCalls(js, tools, "exec_command", "function")
	if len(calls) != 2 {
		t.Fatalf("应解析出 2 个调用（未知工具丢弃）: %+v", calls)
	}
	if !calls[0].IsExec || calls[0].Name != "exec_command" {
		t.Fatalf("第一条应是 exec: %+v", calls[0])
	}
	if calls[1].Name != "mcp__ctx7__query_docs" {
		t.Fatalf("第二条应是 MCP: %+v", calls[1])
	}
	if !json.Valid([]byte(calls[0].ArgsJSON)) || !strings.Contains(calls[0].ArgsJSON, "pwd") {
		t.Fatalf("exec 参数应合法: %s", calls[0].ArgsJSON)
	}
}

// 模型写了 JS 形态（键不带引号）的 MCP 调用也应能解析。
func TestParseBridgeCallsLenientArgs(t *testing.T) {
	tools := registeredClientTools(codexToolsRaw())
	js := "const r = await tools.mcp__ctx7__query_docs({ libraryId: 'x', query: \"y\" });\ntext(r);"
	calls := parseBridgeCalls(js, tools, "exec_command", "function")
	if len(calls) != 1 {
		t.Fatalf("宽松解析失败: %+v", calls)
	}
	var args map[string]any
	if json.Unmarshal([]byte(calls[0].ArgsJSON), &args) != nil || args["libraryId"] != "x" {
		t.Fatalf("宽松参数应转成合法 JSON: %s", calls[0].ArgsJSON)
	}
}

// 放行申请透传：桥面别名 needs_approval 必须翻译成客户端认的
// sandbox_permissions + justification，否则弹不出批准框；直接写
// sandbox_permissions 的（JSON 路径）也照常透传。
func TestToFunctionArgumentsEscalation(t *testing.T) {
	js := "const out = await tools.exec_command({ cmd: \"npm install\", needs_approval: \"Install dependencies with npm?\" });"
	args := toFunctionArguments(js)
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		t.Fatalf("不是合法 JSON: %s", args)
	}
	if m["cmd"] != "npm install" {
		t.Errorf("cmd 提取错误: %v", m["cmd"])
	}
	if m["sandbox_permissions"] != "require_escalated" {
		t.Errorf("needs_approval 未翻译成 require_escalated: %v", m["sandbox_permissions"])
	}
	if m["justification"] != "Install dependencies with npm?" {
		t.Errorf("justification 未翻译: %v", m["justification"])
	}

	// 对象形态：{ justification, prefix_rule }
	js2 := `const o = await tools.exec_command({ cmd: "git pull", needs_approval: { justification: "Pull latest changes?", prefix_rule: ["git", "pull"] } });`
	var m2 map[string]any
	if json.Unmarshal([]byte(toFunctionArguments(js2)), &m2) != nil {
		t.Fatalf("对象形态解析失败")
	}
	if m2["sandbox_permissions"] != "require_escalated" || m2["justification"] != "Pull latest changes?" {
		t.Errorf("对象形态翻译错误: %v", m2)
	}
	pr, ok := m2["prefix_rule"].([]any)
	if !ok || len(pr) != 2 || pr[0] != "git" {
		t.Errorf("prefix_rule 未透传: %v", m2["prefix_rule"])
	}

	// JSON 路径直写 sandbox_permissions 的照常透传。
	direct := toFunctionArguments(`{"cmd":"x","sandbox_permissions":"require_escalated","justification":"Allow?"}`)
	var md map[string]any
	if json.Unmarshal([]byte(direct), &md) != nil || md["sandbox_permissions"] != "require_escalated" {
		t.Errorf("直写路径应原样透传: %s", direct)
	}

	// 非法枚举值必须被清洗掉，不能污染客户端的 serde 反序列化。
	bad := toFunctionArguments(`{"cmd":"x","sandbox_permissions":"with_additional_permissions"}`)
	var mb map[string]any
	if json.Unmarshal([]byte(bad), &mb) != nil {
		t.Fatalf("bad 不是合法 JSON: %s", bad)
	}
	if _, exists := mb["sandbox_permissions"]; exists {
		t.Errorf("未启用的枚举值应被丢弃: %s", bad)
	}
}

// 历史回放：放行参数渲染回 needs_approval 别名（不写 sandbox_permissions，
// 免得模型在历史里看到被禁的参数名又弃权）；非 exec 工具按 tools.<name>(…) 渲染。
func TestReplayCallTextWithOptions(t *testing.T) {
	got := replayCallText("exec_command", `{"cmd":"npm install","sandbox_permissions":"require_escalated","justification":"Install dependencies with npm?"}`)
	if !strings.Contains(got, `needs_approval: "Install dependencies with npm?"`) {
		t.Errorf("放行参数应渲染为 needs_approval: %s", got)
	}
	if strings.Contains(got, "sandbox_permissions") {
		t.Errorf("回放不应出现 sandbox_permissions 字样: %s", got)
	}
	mcp := replayCallText("mcp__ctx7__query_docs", `{"libraryId":"x","query":"y"}`)
	if !strings.Contains(mcp, "await tools.mcp__ctx7__query_docs({") {
		t.Errorf("MCP 调用应按原名渲染: %s", mcp)
	}
}

// 合并上游 goja 求值后的嫁接点：混排块按顺序分组（shell 合成、MCP 独立），
// 变量引用的 MCP 参数按 JS 语义取值，exec 的 needs_approval 出口还原成 sandbox_permissions。
func TestBridgeToolCallsGojaGrouping(t *testing.T) {
	turn := &responsesTurn{
		execToolName: "exec_command", execKind: "function",
		clientTools: registeredClientTools(codexToolsRaw()),
	}
	js := "const lib = '/vercel/' + 'next.js';\n" +
		"await tools.exec_command({ cmd: 'pwd' });\n" +
		"await tools.exec_command({ cmd: 'ls' });\n" +
		"const r = await tools.mcp__ctx7__query_docs({ libraryId: lib, query: `route ${'handlers'}` });\n" +
		"await tools.exec_command({ cmd: 'npm install', needs_approval: 'Install deps?' });\n" +
		"text(r);"
	calls := bridgeToolCalls(turn, js, false)
	if len(calls) != 3 {
		t.Fatalf("应分 3 组（pwd+ls 合成 / MCP / npm），得到 %d: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[0].item, `pwd\\nls`) {
		t.Errorf("连续 shell 调用应合成一条顺序命令: %s", calls[0].item)
	}
	if !strings.Contains(calls[1].item, `"name":"mcp__ctx7__query_docs"`) ||
		!strings.Contains(calls[1].item, `/vercel/next.js`) || !strings.Contains(calls[1].item, `route handlers`) {
		t.Errorf("MCP 参数应按 JS 语义求值: %s", calls[1].item)
	}
	if !strings.Contains(calls[2].item, `sandbox_permissions`) || !strings.Contains(calls[2].item, `require_escalated`) ||
		strings.Contains(calls[2].item, `needs_approval`) {
		t.Errorf("needs_approval 应还原为 sandbox_permissions: %s", calls[2].item)
	}

	// 纯 shell 块不受影响：走上游原路径。
	plain := bridgeToolCalls(turn, "await tools.exec_command({ cmd: 'echo hi' });", false)
	if len(plain) != 1 || !strings.Contains(plain[0].item, "echo hi") {
		t.Errorf("纯 shell 块应走原路径: %+v", plain)
	}
}

func TestFormatSummaryHeading(t *testing.T) {
	cases := map[string]string{
		"**Preparing pages** I need to check.": "**Preparing pages**\n\nI need to check.",
		"**Only title**":                       "**Only title**",
		"plain text no heading":                "plain text no heading",
		"**unclosed heading text":              "**unclosed heading text",
	}
	for in, want := range cases {
		if got := formatSummaryHeading(in); got != want {
			t.Errorf("formatSummaryHeading(%q) = %q, want %q", in, got, want)
		}
	}
}

// 思考事件字段必须满足 Codex 的解析要求（delta+summary_index / item_id+text+summary_index）。
func TestReasoningSummaryEvents(t *testing.T) {
	ev := string(AppendResponsesEvent(nil, ResponsesEvent{Type: "response.reasoning_summary_text.delta", ItemID: "rs_1", Text: "hi"}))
	for _, want := range []string{`"item_id":"rs_1"`, `"summary_index":0`, `"delta":"hi"`} {
		if !strings.Contains(ev, want) {
			t.Errorf("delta 事件缺 %s: %s", want, ev)
		}
	}
	done := string(AppendResponsesEvent(nil, ResponsesEvent{Type: "response.reasoning_summary_text.done", ItemID: "rs_1", Text: "hi"}))
	for _, want := range []string{`"item_id":"rs_1"`, `"summary_index":0`, `"text":"hi"`} {
		if !strings.Contains(done, want) {
			t.Errorf("done 事件缺 %s: %s", want, done)
		}
	}
}

// namespace 组里的工具：给模型看完整名，回给客户端拆回 namespace + 组内名字 ——
// Codex 按这两段找 handler，只给完整名会回 "unsupported call"。回放历史时再拼回完整名。
func TestBridgeToolCallsNamespaced(t *testing.T) {
	raw := map[string]json.RawMessage{"tools": json.RawMessage(`[
 {"type":"custom","name":"exec","description":"Run JS"},
 {"type":"namespace","name":"mcp__gugu","tools":[
  {"type":"function","name":"session_open_in_tab","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}
 ]}
]`)}
	turn := &responsesTurn{execToolName: "exec", execKind: "custom", clientTools: registeredClientTools(raw)}
	if len(turn.clientTools) != 1 || turn.clientTools[0].Name != "mcp__gugu__session_open_in_tab" {
		t.Fatalf("应登记完整名: %+v", turn.clientTools)
	}
	calls := bridgeToolCalls(turn, "const r = await tools.mcp__gugu__session_open_in_tab({ path: 'a.html' });\ntext(r);", false)
	if len(calls) != 1 {
		t.Fatalf("应得 1 条调用: %+v", calls)
	}
	var item struct{ Type, Namespace, Name, Arguments string }
	if err := json.Unmarshal([]byte(calls[0].item), &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "function_call" || item.Namespace != "mcp__gugu" || item.Name != "session_open_in_tab" ||
		!strings.Contains(item.Arguments, "a.html") {
		t.Fatalf("应拆成 namespace + 组内名字: %s", calls[0].item)
	}

	// 平铺工具不带 namespace。
	flat := bridgeToolCalls(&responsesTurn{execToolName: "exec_command", execKind: "function",
		clientTools: registeredClientTools(codexToolsRaw())},
		"await tools.mcp__ctx7__query_docs({ libraryId: 'x', query: 'y' });", false)
	if len(flat) != 1 || strings.Contains(flat[0].item, `"namespace"`) || !strings.Contains(flat[0].item, `"name":"mcp__ctx7__query_docs"`) {
		t.Fatalf("平铺工具应保持原名、不带 namespace: %+v", flat)
	}

	// 客户端回传的历史带 namespace：回放成完整名。
	hist := `[{"type":"function_call","call_id":"c1","namespace":"mcp__gugu","name":"session_open_in_tab","arguments":"{\"path\":\"a.html\"}"},
 {"type":"function_call_output","call_id":"c1","output":"opened"}]`
	var got strings.Builder
	for _, it := range bridgeInputItems(json.RawMessage(hist), "", nil) {
		got.WriteString(itemText(it))
	}
	if !strings.Contains(got.String(), "tools.mcp__gugu__session_open_in_tab(") {
		t.Fatalf("回放应拼回完整名: %s", got.String())
	}
}

func TestJoinToolName(t *testing.T) {
	for _, c := range [][3]string{{"", "a", "a"}, {"mcp__gugu", "x", "mcp__gugu__x"}, {"mcp__gugu__", "x", "mcp__gugu__x"}} {
		if got := joinToolName(c[0], c[1]); got != c[2] {
			t.Errorf("joinToolName(%q,%q)=%q want %q", c[0], c[1], got, c[2])
		}
	}
}

// 打断提示留在时间线上、不进 system：system 不因打断而变（否则原生续接要整份重发 system）。
func TestTurnAbortedStaysInTimeline(t *testing.T) {
	base := `{"type":"message","role":"developer","content":[{"type":"input_text","text":"RULES"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}`
	aborted := `,{"type":"message","role":"developer","content":[{"type":"input_text","text":"<turn_aborted>\nThe previous turn was interrupted on purpose.\n</turn_aborted>"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}`
	before := bridgeInputItems(json.RawMessage("["+base+"]"), "", nil)
	after := bridgeInputItems(json.RawMessage("["+base+aborted+"]"), "", nil)
	if itemText(before[0]) != itemText(after[0]) {
		t.Fatal("打断提示不应改变 system")
	}
	var timeline strings.Builder
	for _, it := range after[1:] {
		timeline.WriteString(itemText(it) + "\n")
	}
	if !strings.Contains(timeline.String(), turnAbortedNote) || strings.Index(timeline.String(), turnAbortedNote) > strings.Index(timeline.String(), "again") {
		t.Fatalf("打断说明应在原位: %s", timeline.String())
	}
}

// 工具结果里的图片（view_image）随 [CLIENT RESULT] 交给上游，执行提醒接在文本块上而不是图片块上。
func TestToolOutputImagesForwarded(t *testing.T) {
	hist := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"看图"}]},
 {"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.view_image({path:'a.png'})"},
 {"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]`
	items := bridgeInputItems(json.RawMessage(hist), "", nil)
	last := items[len(items)-1]
	var img, txt int
	for _, c := range last.Content {
		switch c.Type {
		case "input_image":
			img++
			if c.ImageURL != "data:image/png;base64,iVBORw0KGgo=" {
				t.Fatalf("图片地址不对: %q", c.ImageURL)
			}
		default:
			txt++
			if !strings.Contains(c.Text, "Script completed") || !strings.HasSuffix(c.Text, localExecReminder) {
				t.Fatalf("文本块应含结果并以执行提醒结尾: %q", c.Text)
			}
		}
	}
	if img != 1 || txt != 1 {
		t.Fatalf("应有 1 个图片块和 1 个文本块: %+v", last.Content)
	}
	if conv := itemsConversation(items); conv == nil || strings.Contains(conv.currentText, "LOCAL_EXECUTION_REMINDER") {
		t.Fatal("比对用的本轮文本不应带执行提醒")
	}
}
