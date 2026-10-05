package facade

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// chunkText：每段不超过 size、不切开多字节字符，拼回来就是原文（单行超长也不丢内容）。
func TestChunkText_LosslessWithinSize(t *testing.T) {
	cases := []string{
		strings.Repeat("长", 5000),                                    // 一整行，没有换行可切
		strings.Repeat("line 内容\n", 2000),                            // 多行
		strings.Repeat("x", 3000) + "\n" + strings.Repeat("长", 3000), // 短行 + 超长行
	}
	for _, s := range cases {
		parts := chunkText(s, 4096)
		for _, p := range parts {
			if len(p) > 4096 || !utf8.ValidString(p) {
				t.Fatalf("段长 %d 或切开了多字节字符", len(p))
			}
		}
		if strings.Join(parts, "") != s {
			t.Fatal("拼回来应与原文一致")
		}
	}
}

func TestCutTailAtLine(t *testing.T) {
	s := strings.Repeat("前面的内容\n", 500) + "最后的问题"
	head, tail := cutTailAtLine(s, 1000)
	if head+tail != s || len(tail) > 1000 || !strings.HasSuffix(tail, "最后的问题") || !utf8.ValidString(tail) {
		t.Fatalf("尾段不对: %d 字节", len(tail))
	}
	if !strings.HasPrefix(tail, "前面的内容") {
		t.Fatal("应从某一行的开头留起")
	}
}

// splitOversize：放得下原样返回；放不下时每条都在上限内，system 与本轮消息的内容一字不丢，
// 本轮消息里的非文本块（图片附件等）留在最后一条。
func TestSplitOversize(t *testing.T) {
	const limit = 20000
	small := []prism.InputItem{prism.NewSystemItem("RULES"), prism.NewUserItem("hi")}
	if seeds, out := splitOversize(small, limit); seeds != nil || itemText(out[1]) != "hi" {
		t.Fatal("放得下就不该拆")
	}

	sys := "BRIDGE RULES\n" + strings.Repeat("规则\n", 6000)
	user := strings.Repeat("内容 ", 8000) + "\n最后的问题"
	items := []prism.InputItem{prism.NewSystemItem(sys), {
		Type: "message", Role: "user",
		Content: []prism.InputContent{{Type: prism.BlockInputText, Text: user}, {Type: "input_file", ProjectPath: "/prism-uploads/a.png"}},
	}}
	seeds, out := splitOversize(items, limit)
	if len(seeds) == 0 {
		t.Fatal("放不下应拆出补种消息")
	}
	var seededSys, seededUser strings.Builder
	for _, s := range seeds {
		if n := promptBytes(s); n > limit {
			t.Fatalf("补种消息 %d 字节，超过上限", n)
		}
		body := itemText(s[1])
		switch itemText(s[0]) {
		case overflowSystemSeed:
			seededSys.WriteString(body[strings.IndexByte(body, '\n')+1:])
		case overflowUserSeed:
			seededUser.WriteString(body[strings.IndexByte(body, '\n')+1:])
		default:
			t.Fatalf("未知的补种消息: %.60q", itemText(s[0]))
		}
	}
	if n := promptBytes(out); n > limit {
		t.Fatalf("本轮 %d 字节，超过上限", n)
	}
	gotSys := itemText(out[0])
	if !strings.HasPrefix(gotSys, "BRIDGE RULES") || !strings.HasSuffix(gotSys, overflowSystemNote) {
		t.Fatal("system 应保留开头并注明其余已补种")
	}
	if strings.TrimSuffix(gotSys, overflowSystemNote)+seededSys.String() != sys {
		t.Fatal("system 拆开后内容应一字不丢")
	}
	last := out[1].Content
	if len(last) != 2 || last[1].Type != "input_file" {
		t.Fatal("非文本块应留在本轮消息里")
	}
	finalText := last[0].Text
	if !strings.HasSuffix(finalText, "最后的问题") {
		t.Fatal("本轮消息应保留结尾")
	}
	if seededUser.String()+finalText[strings.IndexByte(finalText, '\n')+1:] != user {
		t.Fatal("本轮消息拆开后内容应一字不丢")
	}
}
