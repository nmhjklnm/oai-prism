package facade

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 客户端注册工具（MCP 等）的识别、文档化与调用翻译。
//
// 背景：Codex CLI 把它注册的全部工具（exec_command、write_stdin、apply_patch，
// 以及用户配置的 MCP 服务器工具）随请求发给网关 —— 两条声明路径（顶层 tools /
// input 里的 additional_tools，见 BridgeEnabled）。桥此前只认 exec_command 一族，
// MCP 工具对上游模型完全不可见：模型不知道有这些工具，查文档、搜索只能让
// 客户端用 shell 硬干。
//
// 本文件把注册工具里「非 shell 内建」的部分（MCP 工具等）列进桥指令，并把
// 模型按桥约定输出的 tools.<name>({...}) 调用翻译回 Responses 协议的
// function_call —— 客户端有真正的 handler，会在本地执行。
//
// 命名空间：lite 路径里工具按 namespace 分组
//（{"type":"namespace","name":"mcp__xx","tools":[…]}）；完整工具名是
// <namespace>__<name>，与官方的 mcp__{server}__{tool} 一致。
// 非 lite 路径是平铺数组，名字已带全前缀。

// builtinToolNames 是桥指令已单独文档化的内建工具，不进 CLIENT TOOLS 清单。
var builtinToolNames = map[string]bool{
	"exec": true, "exec_command": true, "shell": true,
	"write_stdin": true, "apply_patch": true,
}

// isExecToolCallName 判断历史里的工具调用名是否 exec 一族。
var isExecToolCallName = map[string]bool{
	"exec": true, "exec_command": true, "shell": true,
}

// clientTool 是一个客户端注册的可调用工具。
type clientTool struct {
	Name   string // 完整名（namespace__tool 或原名）
	Kind   string // "function" | "custom"
	Desc   string // 描述（已截断）
	Args   string // 参数名一行（"q (required), lib (optional)" 形态）
	Schema string // parameters 的原始 JSON（目录里原样给模型，见 toolDocsSection）
}

// registeredClientTools 解析请求里注册的工具（两条路径都认），返回非内建部分。
// 解析失败不报错、返回空 —— 工具清单是增强，不是必需品。
func registeredClientTools(raw map[string]json.RawMessage) []clientTool {
	var out []clientTool
	seen := map[string]bool{}

	add := func(ns, name, kind, desc string, params json.RawMessage) {
		if name == "" || builtinToolNames[name] || seen[name] {
			return
		}
		if ns != "" {
			name = ns + "__" + name
			if builtinToolNames[name] || seen[name] {
				return
			}
		}
		if kind != "function" && kind != "custom" {
			return
		}
		seen[name] = true
		schema := ""
		if len(params) > 0 {
			schema = string(params)
			if runes := []rune(schema); len(runes) > 1500 {
				schema = string(runes[:1500]) + "…"
			}
		}
		out = append(out, clientTool{
			Name:   name,
			Kind:   kind,
			Desc:   firstParagraph(desc, 600),
			Args:   paramsSummary(params),
			Schema: schema,
		})
	}

	// 工具定义的递归走查：namespace 组 → 平铺工具。
	var walk func(ts []json.RawMessage, ns string)
	walk = func(ts []json.RawMessage, ns string) {
		for _, t := range ts {
			var d struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
				InputSchema json.RawMessage `json:"input_schema"`
				Tools       []json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(t, &d) != nil {
				continue
			}
			if d.Type == "namespace" {
				walk(d.Tools, d.Name)
				continue
			}
			params := d.Parameters
			if len(params) == 0 {
				params = d.InputSchema
			}
			add(ns, d.Name, d.Type, d.Description, params)
		}
	}

	// 路径 B：顶层 tools。
	if ts, ok := rawJSONList(raw["tools"]); ok {
		walk(ts, "")
	}
	// 路径 A：input 里的 additional_tools 条目（可能多条）。
	if items, ok := rawJSONList(raw["input"]); ok {
		for _, it := range items {
			var d struct {
				Type  string          `json:"type"`
				Tools []json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(it, &d) != nil || d.Type != "additional_tools" {
				continue
			}
			walk(d.Tools, "")
		}
	}
	return out
}

// rawJSONList 把 JSON 数组字段解析为元素列表。
func rawJSONList(r json.RawMessage) ([]json.RawMessage, bool) {
	if len(r) == 0 || string(r) == "null" {
		return nil, false
	}
	var out []json.RawMessage
	if json.Unmarshal(r, &out) != nil {
		return nil, false
	}
	return out, true
}

// firstParagraph 取描述的第一段并截断。
func firstParagraph(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n\n", 2)[0])
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return s
}

// paramsSummary 把 JSON Schema 参数摘要成一行 "q (required), lib (optional)"。
// 格式沿袭 excel-codex-bridge 的 describeParameterNames —— 那条渠道实测可用。
func paramsSummary(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var d struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(params, &d) != nil || len(d.Properties) == 0 {
		return ""
	}
	req := map[string]bool{}
	for _, r := range d.Required {
		req[r] = true
	}
	names := make([]string, 0, len(d.Properties))
	for name := range d.Properties {
		names = append(names, name)
	}
	sortStrings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if req[name] {
			parts = append(parts, name+" (required)")
		} else {
			parts = append(parts, name+" (optional)")
		}
	}
	s := strings.Join(parts, ", ")
	if runes := []rune(s); len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return s
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// toolDocsSection 生成桥指令里的 CLIENT TOOLS 段（无工具时返回空串）。
//
// 目录格式沿袭 excel-codex-bridge（上一代渠道）的成熟做法：描述与 JSON
// Schema 原样给模型 —— 模型看到的参数说明和官方后端喂的一致，效果比
// 自编摘要好；它的 args 也是原样透传，sandbox_permissions 之类的参数
// 因此天然可达客户端。
//
// 这里加了体积上限：MCP 的 schema 可以非常大（computer-use 一家就超万
// 字节），而上游单条提示词约 100 KiB 封顶、桥 system 固定部分已 37.5 KB。
// 单工具 schema 1500 字符（registeredClientTools 已截）、目录总量 8000
// 字符，超出的工具只列名字。
func toolDocsSection(tools []clientTool) string {
	if len(tools) == 0 {
		return ""
	}
	const budget = 8000
	var sb strings.Builder
	sb.WriteString("[CLIENT TOOLS - FUNCTION CALLING]\n")
	sb.WriteString("Besides exec_command, the client has these registered tools. When one serves the task (docs lookup, web search, ...), call it INSTEAD of shelling out:\n")
	full := 0
	nameOnly := 0
	for i, t := range tools {
		line := "- " + t.Name + " (" + t.Kind + ")"
		if t.Desc != "" {
			line += ": " + t.Desc
		}
		if t.Kind == "function" {
			if t.Args != "" {
				line += ". Arguments: " + t.Args + "."
			}
			if t.Schema != "" {
				line += " JSON Schema: " + t.Schema
			}
		} else {
			line += ". It receives raw text in input."
		}
		if full > 0 && full+len(line) > budget {
			nameOnly = len(tools) - i
			break
		}
		full += len(line)
		sb.WriteString(line + "\n")
	}
	if nameOnly > 0 {
		var names []string
		for _, t := range tools[len(tools)-nameOnly:] {
			names = append(names, t.Name)
		}
		sb.WriteString("(more registered tools, names only: " + strings.Join(names, ", ") + ")\n")
	}
	sb.WriteString("To call one, emit the codex-exec block with:\n")
	sb.WriteString("  const r = await tools.<full_tool_name>({ \"<param>\": <value> });\n")
	sb.WriteString("  text(r);\n")
	sb.WriteString("The argument must be ONE JSON object literal (double-quoted keys and strings). Results arrive in the next [CLIENT RESULT] — never fabricate them.")
	return sb.String()
}

// bridgeCall 是从 codex-exec 块里解析出的一次工具调用。
type bridgeCall struct {
	Name     string
	Kind     string // "function" | "custom"（custom 仅 exec 一族）
	ArgsJSON string
	Input    string // custom 工具的自由文本输入
	IsExec   bool
}

// parseBridgeCalls 把 codex-exec 块里的 tools.X(...) 调用逐个解析。
//
// 只有当块里出现「非内建的注册工具」调用时才返回非空 —— 纯 exec_command 块
// 仍走单调用的老路径（那条路径带了大量兜底加固：裸命令包装、JS 胶水剥离、
// PowerShell 拆分，重写只会引入回归）。exec_kind 为客户端 shell 工具的实际
// 形态（ExecToolKind 的返回值）。
func parseBridgeCalls(js string, tools []clientTool, execName, execKind string) []bridgeCall {
	registered := map[string]string{} // name -> kind
	for _, t := range tools {
		registered[t.Name] = t.Kind
	}
	var out []bridgeCall
	for _, m := range callSites(js) {
		name := m.name
		kind, known := registered[name]
		isExec := isExecToolCallName[name]
		if !isExec && !known {
			continue // 未注册工具：不透传（客户端只会拒绝）
		}
		if isExec {
			out = append(out, bridgeCall{
				Name:     execName,
				Kind:     execKind,
				ArgsJSON: toFunctionArguments(m.args),
				IsExec:   true,
			})
			continue
		}
		if kind == "custom" {
			out = append(out, bridgeCall{Name: name, Kind: "custom", Input: strings.TrimSpace(m.args)})
			continue
		}
		out = append(out, bridgeCall{Name: name, Kind: "function", ArgsJSON: lenientArgsJSON(m.args)})
	}
	// 只有 exec 调用 → 交还老路径。
	hasNonExec := false
	for _, c := range out {
		if !c.IsExec {
			hasNonExec = true
			break
		}
	}
	if !hasNonExec {
		return nil
	}
	return out
}

// callSite 是文本里一处 tools.X(…) 调用。
type callSite struct {
	name string
	args string
}

// callSites 扫描 js 里所有 `tools.<ident>(<args>)` 调用点。
//
// 括号配平考虑字符串字面量（'、"、`）与转义；嵌套的 {} [] () 也配平。
// 识别是文本级的：字符串里出现的 "tools.foo(...)" 字样也会被当作调用 ——
// 误识别的后果是一次被客户端拒绝的调用、模型看到报错后自纠，可接受。
func callSites(js string) []callSite {
	var out []callSite
	const prefix = "tools."
	for i := 0; i+len(prefix) <= len(js); i++ {
		if js[i:i+len(prefix)] != prefix {
			continue
		}
		j := i + len(prefix)
		start := j
		for j < len(js) && isToolIdentRune(js[j]) {
			j++
		}
		name := js[start:j]
		if name == "" {
			continue
		}
		for j < len(js) && (js[j] == ' ' || js[j] == '\t') {
			j++
		}
		if j >= len(js) || js[j] != '(' {
			continue
		}
		end, ok := matchBalanced(js, j, '(', ')')
		if !ok {
			continue
		}
		out = append(out, callSite{name: name, args: strings.TrimSpace(js[j+1 : end])})
		i = end
	}
	return out
}

func isToolIdentRune(c byte) bool {
	return c == '_' || c == '-' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// matchBalanced 从 js[start]（须为 open）出发找到配对的 close，返回其下标。
func matchBalanced(js string, start int, open, close byte) (int, bool) {
	depth := 0
	var quote byte
	for i := start; i < len(js); i++ {
		c := js[i]
		if quote != 0 {
			if c == '\\' {
				i++ // 跳过转义字符
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// lenientArgsJSON 把调用参数尽量转成合法 JSON 对象。
// 优先严格解析；失败时按「键: 值」逐个收字符串/数字/布尔/数组。
// 整体只是一段字符串字面量时包成 {"input": "..."}（单参工具常见形态）。
// 全都解析不出时返回 {} —— 一次可见的失败好过一条语法错的 arguments。
func lenientArgsJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	if strings.HasPrefix(s, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil {
			return s
		}
		if m := scanObjectScalars(s); len(m) > 0 {
			if b, err := json.Marshal(m); err == nil {
				return string(b)
			}
		}
		return "{}"
	}
	if v, ok := extractQuotedString(s); ok {
		if b, err := json.Marshal(map[string]any{"input": v}); err == nil {
			return string(b)
		}
	}
	return "{}"
}

// scanObjectScalars 从 JS 形态的对象文本里逐个收「键: 标量值」。
func scanObjectScalars(s string) map[string]any {
	out := map[string]any{}
	for _, m := range reScalarKey.FindAllStringSubmatchIndex(s, -1) {
		key := s[m[2]:m[3]]
		if v, ok := parseScalarValue(s[m[1]:]); ok {
			out[key] = v
		}
	}
	return out
}

// execArgWhitelist 是 exec_command 参数里允许透传给客户端的字段。
// 客户端按 serde 反序列化：未知枚举值/类型不符会让整条调用报错，
// 所以逐一校验、宁丢勿错。sandbox_permissions 只放行两个取值 ——
// with_additional_permissions 需要客户端 Feature 开关，不透传。
//
// needs_approval 是桥面别名：上游容器给自己的 agent 注入了
// "Do not provide the sandbox_permissions for any reason" 的 developer 级
// 禁令（2026-10-05 实测，逐字作废也压不住），模型见到那个参数名就弃权。
// 桥面只教中性名 needs_approval（值=给用户的问句），网关在这里翻译回
// sandbox_permissions: require_escalated + justification。
var execArgWhitelist = []string{
	"sandbox_permissions", "justification", "prefix_rule", "needs_approval",
	"workdir", "timeout_ms", "yield_time_ms", "max_output_tokens", "tty",
}

var reScalarKey = regexp.MustCompile(`["']?([A-Za-z_][A-Za-z0-9_]*)["']?\s*:\s*`)

// extractExecOptions 从 JS 源码里提取白名单内的 exec_command 选项。
func extractExecOptions(js string) map[string]any {
	out := map[string]any{}
	for _, m := range reScalarKey.FindAllStringSubmatchIndex(js, -1) {
		key := js[m[2]:m[3]]
		if !stringIn(execArgWhitelist, key) {
			continue
		}
		rest := js[m[1]:]
		if v, ok := parseScalarValue(rest); ok {
			out[key] = v
		}
	}
	return out
}

// parseScalarValue 解析 rest 开头的标量值：字符串 / 数字 / 布尔 / 字符串数组。
func parseScalarValue(rest string) (any, bool) {
	rest = strings.TrimLeft(rest, " \t\r\n")
	if rest == "" {
		return nil, false
	}
	switch rest[0] {
	case '"', '\'', '`':
		if v, ok := extractQuotedString(rest); ok {
			return v, true
		}
	case '{':
		// 对象值（如 needs_approval: { justification, prefix_rule }）：
		// 取配平后的对象文本，按「键: 标量」递归收一层。
		if end, ok := matchBalanced(rest, 0, '{', '}'); ok {
			if m := scanObjectScalars(rest[:end+1]); len(m) > 0 {
				return m, true
			}
		}
	case '[':
		end := strings.Index(rest, "]")
		if end > 0 {
			var arr []string
			for _, part := range strings.Split(rest[1:end], ",") {
				part = strings.TrimSpace(part)
				if v, ok := extractQuotedString(part); ok && v != "" {
					arr = append(arr, v)
				}
			}
			if len(arr) > 0 && len(arr) <= 8 {
				return arr, true
			}
		}
	default:
		// 布尔或数字：截到值边界。
		i := 0
		for i < len(rest) && strings.IndexByte("-+.eE0123456789truefals", rest[i]) >= 0 {
			i++
		}
		tok := rest[:i]
		if tok == "true" {
			return true, true
		}
		if tok == "false" {
			return false, true
		}
		if n, err := strconv.ParseFloat(tok, 64); err == nil && n > 0 {
			return n, true
		}
	}
	return nil, false
}

// normalizeExecArgs 校验/清洗 exec_command 的参数（map 版，见 execArgWhitelist）。
// 白名单外的键一律丢弃：模型输出里的杂键没有意义，客户端 serde 也只认已知字段。
func normalizeExecArgs(m map[string]any) map[string]any {
	if _, ok := m["cmd"].(string); !ok {
		if v, ok := m["command"].(string); ok {
			m["cmd"] = v
		}
	}
	out := map[string]any{}
	if cmd, ok := m["cmd"]; ok {
		out["cmd"] = cmd
	}
	if v, ok := m["sandbox_permissions"].(string); ok && (v == "use_default" || v == "require_escalated") {
		out["sandbox_permissions"] = v
	}
	if _, exists := out["sandbox_permissions"]; !exists {
		if v, ok := m["needs_approval"]; ok {
			out["sandbox_permissions"] = "require_escalated"
			switch x := v.(type) {
			case string:
				if j := truncateRunesTo(x, 240); j != "" {
					out["justification"] = j
				}
			case map[string]any:
				if j, ok := x["justification"].(string); ok {
					if j = truncateRunesTo(j, 240); j != "" {
						out["justification"] = j
					}
				}
				if pr, ok := x["prefix_rule"]; ok {
					if v, ok2 := validPrefixRule(pr); ok2 {
						out["prefix_rule"] = v
					}
				}
			}
		}
	}
	if v, ok := m["justification"].(string); ok {
		if v = truncateRunesTo(v, 240); v != "" {
			out["justification"] = v
		}
	}
	if p, ok := validPrefixRule(m["prefix_rule"]); ok {
		out["prefix_rule"] = p
	}
	for _, k := range []string{"workdir"} {
		if v, ok := m[k].(string); ok {
			out[k] = v
		}
	}
	for _, k := range []string{"timeout_ms", "yield_time_ms", "max_output_tokens"} {
		if v, ok := m[k].(float64); ok && v > 0 {
			out[k] = v
		}
	}
	if v, ok := m["tty"].(bool); ok {
		out["tty"] = v
	}
	return out
}

// truncateRunesTo 截断到 n 个 rune（空串原样返回）。
func truncateRunesTo(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// validPrefixRule 校验 prefix_rule（字符串数组，1~8 项、每项 ≤64 字符）。
func validPrefixRule(v any) ([]string, bool) {
	var arr []string
	switch p := v.(type) {
	case []any:
		for _, e := range p {
			if s, ok := e.(string); ok && len(s) <= 64 {
				arr = append(arr, s)
			}
		}
	case []string:
		for _, s := range p {
			if len(s) <= 64 {
				arr = append(arr, s)
			}
		}
	}
	if len(arr) == 0 || len(arr) > 8 {
		return nil, false
	}
	return arr, true
}

// normalizeExecArgsJSON 是 normalizeExecArgs 的字符串包装。
func normalizeExecArgsJSON(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	if b, err := json.Marshal(normalizeExecArgs(m)); err == nil {
		return string(b)
	}
	return args
}

// bridgeOutputItem 是一条要发给客户端的工具调用输出条目。
type bridgeOutputItem struct {
	id       string
	kind     string // "function" | "custom"
	itemJSON string // 流式事件用的条目 JSON
	asAny    any    // 同步响应用的对象
	input    string // custom 时给 input.done 事件的原文
}

// bridgeOutputItems 把桥 JS 翻译成 Responses 输出条目。
// 块里有非内建注册工具的调用 → 逐调用翻译（exec 调用也各自成条）；
// 否则走单调用的老路径 —— 那条路径带着裸命令包装、胶水剥离等全部加固。
func bridgeOutputItems(turn *responsesTurn, js string) []bridgeOutputItem {
	if calls := parseBridgeCalls(js, turn.clientTools, turn.execToolName, turn.execKind); len(calls) > 0 {
		out := make([]bridgeOutputItem, 0, len(calls))
		for _, c := range calls {
			id := newID("ctc_")
			if c.Kind == "custom" {
				out = append(out, bridgeOutputItem{
					id: id, kind: "custom", input: c.Input,
					itemJSON: customToolCallItemJSON(id, c.Input, c.Name),
					asAny: map[string]any{
						"id": id, "type": "custom_tool_call", "status": "completed",
						"call_id": id, "name": c.Name, "input": c.Input,
					},
				})
				continue
			}
			out = append(out, bridgeOutputItem{
				id: id, kind: "function",
				itemJSON: functionCallItemJSON(id, c.Name, c.ArgsJSON),
				asAny: map[string]any{
					"id": id, "type": "function_call", "status": "completed",
					"call_id": id, "name": c.Name, "arguments": c.ArgsJSON,
				},
			})
		}
		return out
	}
	id := newID("ctc_")
	if turn.execKind == "function" {
		args := toFunctionArguments(js)
		return []bridgeOutputItem{{
			id: id, kind: "function",
			itemJSON: functionCallItemJSON(id, turn.execToolName, args),
			asAny: map[string]any{
				"id": id, "type": "function_call", "status": "completed",
				"call_id": id, "name": turn.execToolName, "arguments": args,
			},
		}}
	}
	return []bridgeOutputItem{{
		id: id, kind: "custom", input: js,
		itemJSON: customToolCallItemJSON(id, js, turn.execToolName),
		asAny: map[string]any{
			"id": id, "type": "custom_tool_call", "status": "completed",
			"call_id": id, "name": turn.execToolName, "input": js,
		},
	}}
}

func stringIn(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
