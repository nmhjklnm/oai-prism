package facade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 技能清单转附件。
//
// Codex 把本机全部技能（名字 + 描述 + SKILL.md 路径）作为一条 <skills_instructions>
// developer 消息发来，桥把它原样并进 system。技能一多它就是 system 里最大的一块：
// 2026-10-05 实测一个 Codex 客户端带着 235 个技能，这一块 70 KB，首轮合计 109 KB，超过上游
// 单条约 100 KiB 的上限，一句 "hi" 都发不出去（context_length_exceeded）。
//
// 同类项目：free-astra 整块删掉，prism-bridge 不转发长的客户端说明。这里折中：system
// 超过单条上限的 skillsCompactShare 时，清单里每个技能只留名字和路径，带描述的完整清单
// 作为附件登记进项目（prism-uploads/，文件名取内容指纹，同一份只传一次），模型拿不准
// 用哪个技能时去读。没超就原样发 —— 描述是选技能的依据，放得下就带着。
//
// 判据只看 system、不看本轮 user：同一会话的 system 不变，压不压缩就不会逐轮翻转
// （system 指纹一变，原生续接就要整份重发 system，见 native.go）。

// skillsCompactShare 是 system 占单条上限的比例，超过就精简技能清单（剩下的留给本轮消息）。
const skillsCompactShare = 0.6

var (
	skillsBlockRe = regexp.MustCompile(`(?s)<skills_instructions>.*?</skills_instructions>`)
	// skillEntryRe 匹配清单里的一条："- <名字>: <描述> (file: <路径>)"。
	skillEntryRe = regexp.MustCompile(`^- ([^:\n]+): .*\(file: ([^)\n]+)\)\s*$`)
)

// contextFile 是桥改写请求时另行登记进项目的附件。
type contextFile struct {
	Name string // prism-uploads/ 下的文件名
	Data []byte
}

// compactSkills 在 system 超出预算时把技能清单换成精简版，返回改写后的条目与要登记的完整清单。
// 不需要或认不出清单时原样返回、附件为 nil。
func compactSkills(items []prism.InputItem, limit int) ([]prism.InputItem, *contextFile) {
	if limit <= 0 {
		return items, nil
	}
	for i, it := range items {
		if !isSystemRole(it.Role) {
			continue
		}
		for j, c := range it.Content {
			if c.Type != prism.BlockInputText || len(c.Text) <= int(float64(limit)*skillsCompactShare) {
				continue
			}
			block := skillsBlockRe.FindString(c.Text)
			if block == "" {
				continue
			}
			sum := sha256.Sum256([]byte(block))
			name := "codex-skills-" + hex.EncodeToString(sum[:6]) + ".md"
			compact, ok := compactSkillsBlock(block, prism.UploadDir+"/"+name)
			if !ok {
				continue
			}
			out := make([]prism.InputItem, len(items))
			copy(out, items)
			contents := make([]prism.InputContent, len(it.Content))
			copy(contents, it.Content)
			contents[j].Text = strings.Replace(c.Text, block, compact, 1)
			out[i].Content = contents
			return out, &contextFile{Name: name, Data: []byte(block)}
		}
	}
	return items, nil
}

// compactSkillsBlock 去掉每条技能的描述，只留名字和路径，并说明完整清单在附件 rel 里。
// 一条技能都认不出时返回 false（格式变了，宁可原样发）。
func compactSkillsBlock(block, rel string) (string, bool) {
	note := "(Descriptions are omitted here to fit the upstream size limit. The full list, saying what each skill is for, " +
		"is the attached project file `" + rel + "` — read it with your read-only file tool, relative to your sandbox working " +
		"directory, when you need to decide whether a skill applies. The SKILL.md paths below are on the CLIENT machine: " +
		"read them with exec_command as usual.)"
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines)+1)
	n, noted := 0, false
	for _, l := range lines {
		if m := skillEntryRe.FindStringSubmatch(l); m != nil {
			if !noted {
				out = append(out, note)
				noted = true
			}
			out = append(out, "- "+strings.TrimSpace(m[1])+" (file: "+strings.TrimSpace(m[2])+")")
			n++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), n > 0
}

// attachContextFiles 把桥改写出的附件登记进项目；同一份内容只传一次（项目里已有同名文件也不重传）。
// 返回本轮是否有新登记 —— 调用方据此换沙箱，让文件落进工作区。
// 登记失败只记日志：模型读不到完整清单，但精简版里的名字和路径仍可用。
func (r *Runner) attachContextFiles(ctx context.Context, p prism.Principal, accountID, projectID string, files []contextFile) bool {
	if projectID == "" || r.client == nil {
		return false
	}
	var added bool
	for _, f := range files {
		key := uploadKey(accountID, projectID, f.Data)
		if _, ok := r.uploads.Get(key); ok {
			continue
		}
		projectPath, uploaded, err := r.client.EnsureProjectFile(ctx, p, projectID, f.Name, "text/markdown", f.Data)
		if err != nil {
			r.log.Warn("附件登记进项目失败，模型只能看到精简清单", "project", projectID, "file", f.Name, "err", err)
			continue
		}
		r.uploads.Put(key, projectPath)
		if uploaded {
			added = true
			r.uploads.MarkProject(projectID)
			r.log.Info("附件已登记进项目", "project", projectID, "path", projectPath, "bytes", len(f.Data))
		}
	}
	return added
}
