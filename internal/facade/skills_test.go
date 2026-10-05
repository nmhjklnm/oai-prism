package facade

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// skillsBlock 造一份 Codex 形态的技能清单：n 条，每条描述 desc 字节。
func skillsBlock(n, desc int) string {
	var b strings.Builder
	b.WriteString("<skills_instructions>\n## Skills\nA skill is a set of local instructions.\n### Skill roots\n- `r0` = `/Users/x/.codex/skills`\n### Available skills\n")
	for i := range n {
		fmt.Fprintf(&b, "- skill-%d: %s (file: r0/skill-%d/SKILL.md)\n", i, strings.Repeat("d", desc), i)
	}
	b.WriteString("</skills_instructions>")
	return b.String()
}

func systemText(items []prism.InputItem) string {
	for _, it := range items {
		if isSystemRole(it.Role) {
			return it.Content[0].Text
		}
	}
	return ""
}

// system 超过上限的 60%：system 里每条技能只留名字和路径，原样的完整清单交给补种。
func TestCompactSkills_OverBudget(t *testing.T) {
	block := skillsBlock(235, 250)
	sys := "BRIDGE RULES\n\n" + block + "\n\nTAIL"
	items := []prism.InputItem{prism.NewSystemItem(sys), prism.NewUserItem("hi")}

	out, full := compactSkills(items, 98304)
	if full != block {
		t.Fatal("system 超预算应移出原样的完整清单")
	}
	got := systemText(out)
	if float64(len(got)) >= 98304*skillsCompactShare {
		t.Fatalf("精简后 system 仍有 %d 字节", len(got))
	}
	for _, want := range []string{"BRIDGE RULES", "TAIL", "- skill-0 (file: r0/skill-0/SKILL.md)",
		"- skill-234 (file: r0/skill-234/SKILL.md)", "[Client skills list]", "### Skill roots"} {
		if !strings.Contains(got, want) {
			t.Fatalf("精简后缺少 %q", want)
		}
	}
	if strings.Contains(got, strings.Repeat("d", 250)) {
		t.Fatal("精简后不应再带描述")
	}
	if systemText(items) != sys {
		t.Fatal("不应改动调用方的条目")
	}
}

// 放得下就原样发：描述是选技能的依据。
func TestCompactSkills_UnderBudgetUntouched(t *testing.T) {
	items := []prism.InputItem{prism.NewSystemItem("RULES\n\n" + skillsBlock(20, 200)), prism.NewUserItem("hi")}
	out, full := compactSkills(items, 98304)
	if full != "" || systemText(out) != systemText(items) {
		t.Fatal("system 没超预算时不应精简")
	}
}

// 认不出清单格式时宁可原样发；没有清单时也不动。
func TestCompactSkills_UnknownFormatUntouched(t *testing.T) {
	odd := "<skills_instructions>\n" + strings.Repeat("free text about skills\n", 4000) + "</skills_instructions>"
	for _, sys := range []string{odd, strings.Repeat("x", 90000)} {
		items := []prism.InputItem{prism.NewSystemItem(sys)}
		if out, full := compactSkills(items, 98304); full != "" || systemText(out) != sys {
			t.Fatal("认不出的内容不应改写")
		}
	}
}

// 补种：每段都放得下单条上限，按行切、不丢内容，拼回来就是原清单。
func TestSkillsSeedTurns_SplitsWithinLimit(t *testing.T) {
	const limit = 98304
	for _, block := range []string{skillsBlock(235, 250), skillsBlock(800, 250)} {
		seeds := skillsSeedTurns(block, limit)
		if len(seeds) == 0 {
			t.Fatal("应生成补种消息")
		}
		var parts []string
		for k, s := range seeds {
			if n := promptBytes(s); n > limit {
				t.Fatalf("第 %d 段 %d 字节，超过上限", k+1, n)
			}
			head := fmt.Sprintf(skillsSeedHeader, k+1, len(seeds))
			parts = append(parts, strings.TrimPrefix(itemText(s[1]), head))
		}
		if strings.Join(parts, "\n") != block {
			t.Fatal("补种内容拼回来应与原清单一致")
		}
	}
	if skillsSeedTurns("", 98304) != nil {
		t.Fatal("没有清单就不补种")
	}
}
