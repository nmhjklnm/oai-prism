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

// system 超过上限的 60%：每条技能只留名字和路径，完整清单原样成为附件，附件名进精简说明。
func TestCompactSkills_OverBudget(t *testing.T) {
	block := skillsBlock(235, 250)
	sys := "BRIDGE RULES\n\n" + block + "\n\nTAIL"
	items := []prism.InputItem{prism.NewSystemItem(sys), prism.NewUserItem("hi")}

	out, file := compactSkills(items, 98304)
	if file == nil {
		t.Fatal("system 超预算应精简技能清单")
	}
	if string(file.Data) != block {
		t.Fatal("附件应是原样的完整清单")
	}
	got := systemText(out)
	if float64(len(got)) >= 98304*skillsCompactShare {
		t.Fatalf("精简后 system 仍有 %d 字节", len(got))
	}
	for _, want := range []string{"BRIDGE RULES", "TAIL", "- skill-0 (file: r0/skill-0/SKILL.md)",
		"- skill-234 (file: r0/skill-234/SKILL.md)", "prism-uploads/" + file.Name, "### Skill roots"} {
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
	if _, again := compactSkills(items, 98304); again == nil || again.Name != file.Name {
		t.Fatal("同一份清单的附件名应稳定（会话内 system 指纹不变）")
	}
}

// 放得下就原样发：描述是选技能的依据。
func TestCompactSkills_UnderBudgetUntouched(t *testing.T) {
	items := []prism.InputItem{prism.NewSystemItem("RULES\n\n" + skillsBlock(20, 200)), prism.NewUserItem("hi")}
	out, file := compactSkills(items, 98304)
	if file != nil || systemText(out) != systemText(items) {
		t.Fatal("system 没超预算时不应精简")
	}
}

// 认不出清单格式时宁可原样发；没有清单时也不动。
func TestCompactSkills_UnknownFormatUntouched(t *testing.T) {
	odd := "<skills_instructions>\n" + strings.Repeat("free text about skills\n", 4000) + "</skills_instructions>"
	for _, sys := range []string{odd, strings.Repeat("x", 90000)} {
		items := []prism.InputItem{prism.NewSystemItem(sys)}
		if out, file := compactSkills(items, 98304); file != nil || systemText(out) != sys {
			t.Fatal("认不出的内容不应改写")
		}
	}
}
