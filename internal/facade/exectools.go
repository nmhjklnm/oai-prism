package facade

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Codex code mode 把大部分工具嵌在 exec 这一个 custom 工具里：exec 的描述先讲 V8 脚本的规矩与
// 全局 helper，再逐个列出嵌套工具（"### `image_gen__imagegen`" + 说明 + TS 声明）。
// 桥给模型看的每个客户端工具只有描述第一段（registeredClientTools 的 firstParagraph），
// 嵌套工具因此一个都到不了模型 —— 2026-10-05 实测：咕咕给 Codex 打开出图后，exec 描述里
// 已有 image_gen__imagegen，模型仍答「当前会话没有出图工具」。
//
// 这里把嵌套工具的说明原样搬进桥指令（exec_command / apply_patch / write_stdin 桥已单独写过，
// 跳过），外加脚本首行 pragma 与全局 helper 的那几条：出图要靠 pragma 拉长等待、
// 靠 generatedImage 交回图片。模型照常输出 codex-exec 块，块原样交给客户端的 V8
// 执行（shellToolCalls 的 custom 路径），嵌套工具在客户端跑。

const (
	execToolDocMax  = 3000  // 单个嵌套工具说明的上限（字符）
	execToolsBudget = 12000 // 整段上限；超出的工具只列名字
)

var (
	execToolHeadRe = regexp.MustCompile("(?m)^### `([A-Za-z0-9_]+)`[ \t]*$")
	// 嵌套工具的说明到下一个二级标题或分隔线为止（之后是命名空间说明、web 的长篇用法等）。
	execSectionEndRe = regexp.MustCompile(`(?m)^(## |---)`)
	execIntroEndRe   = regexp.MustCompile(`(?m)^#{2,3} `)
)

// execNestedTool 是 exec 描述里列出的一个嵌套工具。
type execNestedTool struct {
	Name string // tools 上的名字（JS 标识符，如 image_gen__imagegen）
	Doc  string // 说明与 TS 声明（已截断）
}

// codeModeExecDescription 找出 code mode 的 exec 工具描述（custom 工具 exec，
// 在顶层 tools 或 additional_tools 的 namespace 里）。没有就返回空串。
func codeModeExecDescription(raw map[string]json.RawMessage) string {
	found := ""
	var walk func(ts []json.RawMessage)
	walk = func(ts []json.RawMessage) {
		for _, t := range ts {
			if found != "" {
				return
			}
			var d struct {
				Type        string            `json:"type"`
				Name        string            `json:"name"`
				Description string            `json:"description"`
				Tools       []json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(t, &d) != nil {
				continue
			}
			if d.Type == "namespace" {
				walk(d.Tools)
				continue
			}
			if d.Type == "custom" && d.Name == "exec" {
				found = d.Description
			}
		}
	}
	if ts, ok := rawJSONList(raw["tools"]); ok {
		walk(ts)
	}
	if items, ok := rawJSONList(raw["input"]); ok {
		for _, it := range items {
			var d struct {
				Type  string            `json:"type"`
				Tools []json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(it, &d) == nil && d.Type == "additional_tools" {
				walk(d.Tools)
			}
		}
	}
	return found
}

// execNestedTools 按 "### `name`" 切出嵌套工具，跳过桥已单独文档化的内建工具。
func execNestedTools(desc string) []execNestedTool {
	heads := execToolHeadRe.FindAllStringSubmatchIndex(desc, -1)
	var out []execNestedTool
	for i, h := range heads {
		name := desc[h[2]:h[3]]
		end := len(desc)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		body := desc[h[1]:end]
		if loc := execSectionEndRe.FindStringIndex(body); loc != nil {
			body = body[:loc[0]]
		}
		if builtinToolNames[name] {
			continue
		}
		body = strings.TrimSpace(body)
		if runes := []rune(body); len(runes) > execToolDocMax {
			body = string(runes[:execToolDocMax]) + "…"
		}
		out = append(out, execNestedTool{Name: name, Doc: body})
	}
	return out
}

// execScriptNotes 取 exec 描述开头里桥没讲过的几条：首行 pragma（yield_time_ms /
// max_output_tokens）与全局 helper 清单。认不出就返回空串。
func execScriptNotes(desc string) string {
	intro := desc
	if loc := execIntroEndRe.FindStringIndex(desc); loc != nil {
		intro = desc[:loc[0]]
	}
	var keep []string
	helpers := false
	for _, line := range strings.Split(intro, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "- Global helpers"):
			helpers = true
			keep = append(keep, t)
		case helpers && strings.HasPrefix(t, "- "):
			keep = append(keep, t)
		case strings.Contains(t, "first-line pragma"),
			strings.HasPrefix(t, "- `yield_time_ms`"),
			strings.HasPrefix(t, "- `max_output_tokens`"):
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "\n")
}

// execToolsSection 生成桥指令里的 CLIENT EXEC TOOLS 段（不是 code mode 或没有嵌套工具时返回空串）。
func execToolsSection(raw map[string]json.RawMessage) string {
	desc := codeModeExecDescription(raw)
	tools := execNestedTools(desc)
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[CLIENT EXEC TOOLS - CALL INSIDE THE codex-exec BLOCK]\n")
	sb.WriteString("The client's exec runtime also provides these tools on the global `tools` object (descriptions are the client's own). ")
	sb.WriteString("Call them INSIDE the codex-exec block — the whole block runs on the client:\n")
	sb.WriteString("  const r = await tools.<name>({ ... });\n")
	if notes := execScriptNotes(desc); notes != "" {
		sb.WriteString("Script rules from the client (a first-line pragma, if used, must be the block's first line):\n")
		sb.WriteString(notes + "\n")
	}
	used := sb.Len()
	var nameOnly []string
	for _, t := range tools {
		entry := "### tools." + t.Name + "\n" + t.Doc + "\n"
		if len(nameOnly) > 0 || used+len(entry) > execToolsBudget {
			nameOnly = append(nameOnly, t.Name)
			continue
		}
		used += len(entry)
		sb.WriteString(entry)
	}
	if len(nameOnly) > 0 {
		sb.WriteString("(more exec tools, names only: " + strings.Join(nameOnly, ", ") + ")\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
