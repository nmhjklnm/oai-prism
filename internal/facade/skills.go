package facade

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 技能清单单独补种。
//
// Codex 把本机全部技能（名字 + 描述 + SKILL.md 路径）作为一条 <skills_instructions>
// developer 消息发来，桥把它并进 system。技能一多它就是 system 里最大的一块：
// 2026-10-05 实测一个 Codex 客户端带着 235 个技能，这一块 70 KB，首轮合计 109 KB，
// 超过上游单条约 100 KiB 的上限，一句 "hi" 都发不出去（context_length_exceeded）。
//
// 这份清单是 Codex 认定模型必须知道的指令，不能删，也不能交给模型"需要时去读"
// （读不读由它决定，等于可能没送到）。所以 system 超过单条上限的 skillsCompactShare
// 时：system 里每个技能只留名字和路径；完整清单在新建上游会话时、本轮之前单独补种
// （一段放不下就分几段），由网关保证送到。上游会话保管历史，之后各轮不再重发。
// 没超就原样留在 system 里。
//
// 判据只看 system、不看本轮 user：同一会话的 system 不变，压不压缩就不会逐轮翻转
// （system 指纹一变，原生续接就要整份重发 system，见 native.go）。

// skillsCompactShare 是 system 占单条上限的比例，超过就把技能清单移出 system（剩下的留给本轮消息）。
const skillsCompactShare = 0.6

const (
	skillsSeedSystem = "The client's skills list follows. It is part of your standing instructions for this whole " +
		"conversation: later turns name these skills and expect you to know what each one is for. Do not act on it now; " +
		"reply with exactly OK."
	skillsSeedHeader = "[Client skills list, part %d of %d]\n"
	// skillsCompactNote 写在 system 精简清单的开头，指向补种进会话的完整清单。
	skillsCompactNote = "(The full skills list, with what each skill is for, was delivered at the start of this conversation " +
		"in the messages headed [Client skills list]. It is part of your instructions and stays in force; the entries " +
		"below repeat only names and paths. The SKILL.md paths are on the CLIENT machine: read them with exec_command as usual.)"
)

var (
	skillsBlockRe = regexp.MustCompile(`(?s)<skills_instructions>.*?</skills_instructions>`)
	// skillEntryRe 匹配清单里的一条："- <名字>: <描述> (file: <路径>)"。
	skillEntryRe = regexp.MustCompile(`^- ([^:\n]+): .*\(file: ([^)\n]+)\)\s*$`)
)

// compactSkills 在 system 超出预算时把技能清单换成精简版，返回改写后的条目与完整清单
// （由原生续接在新建上游会话时补种）。不需要或认不出清单时原样返回、清单为空。
func compactSkills(items []prism.InputItem, limit int) ([]prism.InputItem, string) {
	if limit <= 0 {
		return items, ""
	}
	for i, it := range items {
		if !isSystemRole(it.Role) {
			continue
		}
		for j, c := range it.Content {
			if c.Type != prism.BlockInputText || float64(len(c.Text)) <= float64(limit)*skillsCompactShare {
				continue
			}
			block := skillsBlockRe.FindString(c.Text)
			if block == "" {
				continue
			}
			compact, ok := compactSkillsBlock(block)
			if !ok {
				continue
			}
			out := make([]prism.InputItem, len(items))
			copy(out, items)
			contents := make([]prism.InputContent, len(it.Content))
			copy(contents, it.Content)
			contents[j].Text = strings.Replace(c.Text, block, compact, 1)
			out[i].Content = contents
			return out, block
		}
	}
	return items, ""
}

// compactSkillsBlock 去掉每条技能的描述，只留名字和路径。一条都认不出时返回 false（格式变了，宁可原样发）。
func compactSkillsBlock(block string) (string, bool) {
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines)+1)
	n := 0
	for _, l := range lines {
		if m := skillEntryRe.FindStringSubmatch(l); m != nil {
			if n == 0 {
				out = append(out, skillsCompactNote)
			}
			out = append(out, "- "+strings.TrimSpace(m[1])+" (file: "+strings.TrimSpace(m[2])+")")
			n++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), n > 0
}

// skillsSeedTurns 把完整技能清单切成补种消息，每段不超过单条上限；按行切，不切开一条技能。
func skillsSeedTurns(block string, limit int) [][]prism.InputItem {
	budget := limit - len(skillsSeedSystem) - 64 - promptOverheadReserve
	if block == "" || limit <= 0 || budget < 4096 {
		return nil
	}
	var chunks []string
	var cur strings.Builder
	for _, l := range strings.Split(block, "\n") {
		if len(l) > budget {
			l = cutUTF8(l, budget-32) + " …[truncated]"
		}
		if cur.Len() > 0 && cur.Len()+len(l)+1 > budget {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(l)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	out := make([][]prism.InputItem, 0, len(chunks))
	for k, c := range chunks {
		out = append(out, []prism.InputItem{
			prism.NewSystemItem(skillsSeedSystem),
			prism.NewUserItem(fmt.Sprintf(skillsSeedHeader, k+1, len(chunks)) + c),
		})
	}
	return out
}
