package facade

import (
	"fmt"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 超过上游单条上限的一轮：拆开补种，不报错。
//
// 上游把每轮的 [system, user] 合成一条消息，按 UTF-8 字节限长（约 100 KiB，见 context_limit.go）。
// 这是单条消息的上限，不是上下文窗口：上游会话保管全部历史，窗口大得多。所以放不下就拆开 ——
// 在同一个上游会话里先把超出的部分逐段补种（每段一轮，模型只回 OK），再发本轮。补种由网关
// 保证送到，不依赖模型自己去读附件；2026-10-05 实测 70 KB 一段约 6 秒，附件方案光上传加换
// 沙箱就要约 15 秒，读文件还得再算进模型那一轮。
//
// 拆法：
//   - system 超过 splitSystemShare：保留开头（桥指令在最前），其余作为"常驻指令（续）"补种；
//   - 本轮消息仍放不下：保留结尾（真正要做的事通常在最后），前面按顺序补种。
//
// 技能清单有更好的拆法（system 里留名字和路径，见 skills.go），在这之前执行。

// splitSystemShare 是拆分后 system 最多占单条上限的比例，剩下的留给本轮消息。
const splitSystemShare = 0.6

const (
	overflowSystemSeed   = "More of your standing instructions for this conversation follow. They bind you exactly like the rest of your instructions. Do not act on them now; reply with exactly OK."
	overflowSystemHeader = "[Standing instructions, continued - part %d of %d]\n"
	overflowSystemNote   = "\n\n(The rest of your standing instructions was delivered earlier in this conversation, in the messages headed [Standing instructions, continued]. It stays in force.)"
	overflowUserSeed     = "The user's message for this turn is long and arrives in several parts. Read this part; do not act on it yet; reply with exactly OK."
	overflowUserHeader   = "[The user's message for this turn, part %d of %d]\n"
	overflowUserFinal    = "[The user's message for this turn, part %d of %d. The earlier parts were sent just before this one: now act on the whole message.]\n"
)

// splitOversize 把超过单条上限的 [system, user] 拆成补种消息与放得下的本轮条目。
// 放得下、不是 [system, user] 形状、或拆完仍放不下时原样返回，seeds 为空。
func splitOversize(items []prism.InputItem, limit int) (seeds [][]prism.InputItem, out []prism.InputItem) {
	if limit <= 0 || promptBytes(items) <= limit || len(items) != 2 || !isSystemRole(items[0].Role) {
		return nil, items
	}
	sys := itemText(items[0])
	if sysMax := int(float64(limit) * splitSystemShare); len(sys) > sysMax {
		head, rest := cutAtLine(sys, sysMax-len(overflowSystemNote))
		seeds = append(seeds, overflowSeeds(rest, overflowSystemSeed, overflowSystemHeader, 0, limit)...)
		sys = head + overflowSystemNote
	}

	user := items[1]
	var texts []string
	var other []prism.InputContent
	for _, c := range user.Content {
		if c.Type == prism.BlockInputText {
			texts = append(texts, c.Text)
		} else {
			other = append(other, c)
		}
	}
	text := strings.Join(texts, "\n\n")
	room := limit - len(sys) - promptOverheadReserve - len(overflowUserFinal) - 16
	if len(text) > room {
		if room < 4096 {
			return nil, items
		}
		head, tail := cutTailAtLine(text, room)
		parts := overflowSeeds(head, overflowUserSeed, overflowUserHeader, 1, limit)
		seeds = append(seeds, parts...)
		n := len(parts) + 1
		text = fmt.Sprintf(overflowUserFinal, n, n) + tail
	}

	cur := prism.InputItem{Type: user.Type, Role: user.Role, ID: user.ID,
		Content: append([]prism.InputContent{{Type: prism.BlockInputText, Text: text}}, other...)}
	out = []prism.InputItem{prism.NewSystemItem(sys), cur}
	if promptBytes(out) > limit {
		return nil, items
	}
	return seeds, out
}

// overflowSeeds 把 text 切成补种消息：每段一条 [seedSystem, header + 段]。extra 是排在这些段之后、
// 计入总段数的段数（本轮消息的最后一段随本轮发出）。
func overflowSeeds(text, seedSystem, header string, extra, limit int) [][]prism.InputItem {
	parts := chunkText(text, seedBudget(seedSystem, header, limit))
	out := make([][]prism.InputItem, 0, len(parts))
	for k, p := range parts {
		out = append(out, []prism.InputItem{prism.NewSystemItem(seedSystem),
			prism.NewUserItem(fmt.Sprintf(header, k+1, len(parts)+extra) + p)})
	}
	return out
}

// seedBudget 是一条补种消息里正文的字节预算。
func seedBudget(seedSystem, header string, limit int) int {
	return limit - len(seedSystem) - len(header) - 16 - promptOverheadReserve
}

// chunkText 按行把 s 切成每段不超过 size 字节的若干段；单行超长时在 UTF-8 边界硬切（不丢内容）。
func chunkText(s string, size int) []string {
	if s == "" || size <= 0 {
		return nil
	}
	var out []string
	for len(s) > 0 {
		if len(s) <= size {
			out = append(out, s)
			break
		}
		head, rest := cutAtLine(s, size)
		out = append(out, head)
		s = rest
	}
	return out
}

// cutAtLine 在不超过 n 字节处切开 s，尽量切在换行之后；返回前后两段，拼起来就是 s。
func cutAtLine(s string, n int) (string, string) {
	if n <= 0 {
		return "", s
	}
	if len(s) <= n {
		return s, ""
	}
	cut := len(cutUTF8(s, n))
	if i := strings.LastIndexByte(s[:cut], '\n'); i >= cut/2 {
		cut = i + 1
	}
	return s[:cut], s[cut:]
}

// cutTailAtLine 从 s 的结尾留下不超过 n 字节，尽量从某一行的开头留起；返回前后两段，拼起来就是 s。
func cutTailAtLine(s string, n int) (string, string) {
	if len(s) <= n {
		return "", s
	}
	start := len(s) - n
	for start < len(s) && !utf8RuneStart(s[start]) {
		start++
	}
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 && i < n/2 {
		start += i + 1
	}
	return s[:start], s[start:]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
