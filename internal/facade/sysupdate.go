package facade

import (
	"strings"
)

// system 内容变了时只发变化。
//
// 原生续接下 system 只在会话开头完整发一次。客户端中途改了指令（换了工作目录、装了新技能、
// 工具清单变了）时，以前整份重发：Codex 的 system 带着技能清单和工具目录，一次几十 KB，
// 全都留在上游会话里挤占窗口。现在按行找出从第一处变化到最后一处变化的那一段，只发这一段
// 并说明它在原文里的位置；变化占了大半时才整份重发。

const (
	sysUpdateHead = "[Update to your standing instructions]\n" +
		"Part of the instructions given earlier in this conversation has changed. Apply this change; " +
		"everything else in those instructions stays in force.\n"
	// sysUpdateQuoteMax 是旧内容原样引出的上限：短的照抄，模型能确切知道换掉了什么；长的只给位置。
	sysUpdateQuoteMax = 2 << 10
	// sysAnchorMax 是位置锚点（变化前后那一行）的长度上限。
	sysAnchorMax = 160
)

// systemUpdate 返回把 old 改成 new 的更新说明。没有旧文本、没有变化、或更新说明不比整份
// 重发小一半时返回 false（调用方整份重发）。
func systemUpdate(old, new string) (string, bool) {
	if old == "" || old == new {
		return "", false
	}
	ol, nl := strings.Split(old, "\n"), strings.Split(new, "\n")
	p := 0
	for p < len(ol) && p < len(nl) && ol[p] == nl[p] {
		p++
	}
	s := 0
	for s < len(ol)-p && s < len(nl)-p && ol[len(ol)-1-s] == nl[len(nl)-1-s] {
		s++
	}
	oldMid := strings.Join(ol[p:len(ol)-s], "\n")
	newMid := strings.Join(nl[p:len(nl)-s], "\n")

	var sb strings.Builder
	sb.WriteString(sysUpdateHead)
	before, after := sysAnchor(ol[:p], true), sysAnchor(ol[len(ol)-s:], false)
	switch {
	case before != "" && after != "":
		sb.WriteString("Where: between the line starting \"" + before + "\" and the line starting \"" + after + "\".\n")
	case before != "":
		sb.WriteString("Where: after the line starting \"" + before + "\", to the end.\n")
	case after != "":
		sb.WriteString("Where: from the beginning, up to the line starting \"" + after + "\".\n")
	}
	if oldMid != "" {
		if len(oldMid) <= sysUpdateQuoteMax {
			sb.WriteString("Old text there (now void):\n<<<\n" + oldMid + "\n>>>\n")
		} else {
			sb.WriteString("The old text there is now void.\n")
		}
	}
	if newMid != "" {
		sb.WriteString("New text there:\n<<<\n" + newMid + "\n>>>")
	} else {
		sb.WriteString("That part is removed; nothing replaces it.")
	}
	upd := sb.String()
	if len(upd) > len(new)/2 {
		return "", false
	}
	return upd, true
}

// sysAnchor 取紧挨变化处的一行非空文本当位置锚点：before=true 取 lines 里最后一行，否则取第一行。
func sysAnchor(lines []string, before bool) string {
	for k := range lines {
		i := k
		if before {
			i = len(lines) - 1 - k
		}
		if t := strings.TrimSpace(lines[i]); t != "" {
			return truncateRunes(t, sysAnchorMax)
		}
	}
	return ""
}
