package facade

import (
	"encoding/json"
	"strings"
	"testing"
)

// codeModeExecDesc 是 Codex 0.160 code mode 下 exec 描述的缩写（结构照 2026-10-05 抓到的原文：
// 开头的脚本规矩与全局 helper → 逐个嵌套工具 → 命名空间 → 分隔线后的 web 长篇用法）。
// ‵ 代替反引号（Go 原始字符串里放不了）。
var codeModeExecDesc = strings.ReplaceAll(`Run JavaScript code to orchestrate/compose tool calls
- Evaluates the provided JavaScript code in a fresh V8 isolate as an async module.
- All nested tools are available on the global ‵tools‵ object, for example ‵await tools.exec_command(...)‵.
- You may optionally start the tool input with a first-line pragma like ‵// @exec: {"yield_time_ms": 10000, "max_output_tokens": 1000}‵.
- ‵yield_time_ms‵ asks ‵exec‵ to yield early if the script is still running. Defaults to 30000 ms.
- ‵max_output_tokens‵ sets the token budget for direct ‵exec‵ results. Defaults to 10000 tokens.

- Global helpers:
- ‵text(value)‵: Appends a text item.
- ‵generatedImage(result: { image_url: string; output_hint?: string })‵: Appends an image-generation result and its optional output hint.

### ‵apply_patch‵
Use the ‵apply_patch‵ tool to edit files.

### ‵exec_command‵
Runs a command in a PTY.

### ‵view_image‵
View a local image from the filesystem.

## clock
Tools for reading and waiting on time.

### ‵clock__curr_time‵
Return the current time in UTC.

## image_gen
Tools in the image_gen namespace.

### ‵image_gen__imagegen‵
The ‵image_gen.imagegen‵ tool enables image generation from descriptions. Use it when:

- The user requests an image based on a scene description.

Guidelines:
- imagegen needs a few minutes to finish. In code-mode, use the first-line @exec directive to give the initial call 120 seconds. Once it finishes, return the image with generatedImage(result).

exec tool declaration:
‵‵‵ts
declare const tools: { image_gen__imagegen(args: {
  prompt: string;
  transparent_background?: boolean;
}): Promise<unknown>; };
‵‵‵

## web
Tools in the web namespace.

### ‵web__run‵
Tool for accessing the internet.

---

## Examples of different commands available in this tool
* ‵search_query‵: {"search_query": [{"q": "WEB-USAGE-MARKER"}]}`, "‵", "`")

// liteRequest 是 lite 路径的请求：工具在 input 的 additional_tools 里，exec 在 functions 命名空间下。
func liteRequest(t *testing.T, execType, desc string) map[string]json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol",
		"input": []any{
			map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
				map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": execType, "name": "exec", "description": desc},
					map[string]any{"type": "function", "name": "wait", "description": "Wait for exec.", "parameters": map[string]any{"type": "object"}},
				}},
			}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "画一个红苹果"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// exec 的嵌套工具（出图等）进桥指令：说明与 TS 声明原样，外加首行 pragma 与 helper。
func TestExecToolsSection_NestedToolsReachModel(t *testing.T) {
	sec := execToolsSection(liteRequest(t, "custom", codeModeExecDesc))
	for _, want := range []string{
		"[CLIENT EXEC TOOLS",
		"### tools.image_gen__imagegen",
		"declare const tools: { image_gen__imagegen(args: {",
		"give the initial call 120 seconds",
		"### tools.view_image",
		"### tools.clock__curr_time",
		"first-line pragma",
		"`yield_time_ms` asks",
		"generatedImage(result:",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("缺少 %q\n%s", want, sec)
		}
	}
	// 桥已单独写过的内建工具不重复；嵌套工具的说明止于下一个二级标题，后面的长篇用法不进来。
	for _, bad := range []string{"### tools.exec_command", "### tools.apply_patch", "WEB-USAGE-MARKER", "Tools in the image_gen namespace"} {
		if strings.Contains(sec, bad) {
			t.Errorf("不该有 %q", bad)
		}
	}
}

// 阳性对照：改之前模型只看得到 CLIENT TOOLS（每个工具描述的第一段），里面没有嵌套工具。
func TestExecToolsSection_ClientToolsAloneMissNestedTools(t *testing.T) {
	docs := toolDocsSection(registeredClientTools(liteRequest(t, "custom", codeModeExecDesc)))
	if strings.Contains(docs, "image_gen__imagegen") {
		t.Fatalf("CLIENT TOOLS 本来就列出了出图工具，这条修复的前提不成立:\n%s", docs)
	}
}

// 不是 code mode（没有 custom 的 exec）就不加这一段。
func TestExecToolsSection_NotCodeMode(t *testing.T) {
	if sec := execToolsSection(liteRequest(t, "function", codeModeExecDesc)); sec != "" {
		t.Fatalf("function 形态的 exec 不是 code mode，应为空:\n%s", sec)
	}
	if sec := execToolsSection(liteRequest(t, "custom", "Run JavaScript code.")); sec != "" {
		t.Fatalf("没有嵌套工具应为空:\n%s", sec)
	}
}

// 超出总量上限的工具只列名字，不静默丢掉。
func TestExecToolsSection_BudgetListsNames(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Run JavaScript code.\n\n")
	for i := 0; i < 12; i++ {
		sb.WriteString("### `tool_" + string(rune('a'+i)) + "`\n" + strings.Repeat("x", 2000) + "\n\n")
	}
	sec := execToolsSection(liteRequest(t, "custom", sb.String()))
	if !strings.Contains(sec, "### tools.tool_a") || !strings.Contains(sec, "names only: ") || !strings.Contains(sec, "tool_l") {
		t.Fatalf("超限时应先给全文、其余列名字:\n%.400s", sec)
	}
	if len(sec) > execToolsBudget+500 {
		t.Fatalf("段落 %d 字节，超过上限", len(sec))
	}
}

// 接到桥指令里：execDocs 出现在 <local_tool_bridge> 之内。
func TestBridgePrompt_IncludesExecDocs(t *testing.T) {
	p := bridgePrompt(true, nil, "[CLIENT EXEC TOOLS - X]")
	if i, j := strings.Index(p, "[CLIENT EXEC TOOLS - X]"), strings.Index(p, "</local_tool_bridge>"); i < 0 || i > j {
		t.Fatalf("exec 工具段应在桥指令之内")
	}
}
