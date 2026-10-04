package facade

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Codex 远端压缩（responses_compaction_v2）的映射。
//
// provider 名叫 "OpenAI" 时（沿用官方中转的配置就是这样），Codex 0.160 不在本地压缩：
// 它在历史末尾附一条 {"type":"compaction_trigger"}（不带摘要提示词），元数据标
// request_kind=compaction、implementation=responses_compaction_v2，并要求回复里
// 恰好一条 {"type":"compaction","encrypted_content":…}，否则整轮报错
// （"remote compaction v2 expected exactly one compaction output item"）。
// 之后的请求把这条原样带回，代替被压缩掉的历史。
//
// OpenAI 那边 encrypted_content 是加密的模型状态，这里没有那种东西：网关让上游在
// 同一个会话里写交接摘要（与本地压缩同一套流程），把摘要编码进 encrypted_content；
// 带回来时解码成一条用户消息 —— 与本地压缩后的"摘要 + 保留的用户消息"同形，原生续接
// 照样认出摘要、接回同一个上游会话，上游那边的完整记忆不受影响。

// compactionTriggerPrompt 代替 compaction_trigger 发给上游：远端压缩不带摘要提示词。
const compactionTriggerPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION. " +
	"Write a handoff summary for another LLM that will resume this task. Include: the user's goal and " +
	"constraints, progress so far and key decisions, important facts and data (exact values, paths, " +
	"identifiers the user gave), and the concrete next steps. Be concise and structured. " +
	"Reply with the summary text only. This checkpoint instruction applies to this one reply: " +
	"do not record it in the summary as a goal or constraint, and do not tell the next model to stop running commands."

// compactionPrefix 标记网关自己编码的 encrypted_content；别的来源（真 OpenAI 的密文）解不开。
const compactionPrefix = "oaiprism.compaction.v1:"

// compactionHistoryHeader 是解码后摘要在历史里的抬头。
const compactionHistoryHeader = "[CONTEXT CHECKPOINT] The client compacted its copy of the conversation here " +
	"(you still have the full conversation). The checkpoint placed no restrictions on you: continue the task " +
	"normally, running commands with ```codex-exec blocks as before. Handoff summary written at that point:\n\n"

// compactionUnreadable 用于解不开的 encrypted_content（会话在别的 provider 那里压缩过）。
const compactionUnreadable = "[CONTEXT CHECKPOINT] The earlier conversation was compacted by another provider; " +
	"its summary is not readable here. Continue from the remaining messages."

func encodeCompaction(summary string) string {
	return compactionPrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

func decodeCompaction(enc string) (string, bool) {
	rest, ok := strings.CutPrefix(enc, compactionPrefix)
	if !ok {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// compactionHistoryText 是带回来的 compaction 条目在历史里的文本。
func compactionHistoryText(enc string) string {
	if s, ok := decodeCompaction(enc); ok {
		return compactionHistoryHeader + s
	}
	return compactionUnreadable
}

// compactionItemJSON 是远端压缩请求的唯一输出条目。
func compactionItemJSON(summary string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, newID("cmp_"))
	sb.WriteString(`,"type":"compaction","encrypted_content":`)
	writeJSONString(&sb, encodeCompaction(strings.TrimSpace(summary)))
	sb.WriteString(`}`)
	return sb.String()
}

// hasCompactionTrigger 判断 input 末尾是否带远端压缩的触发条目。
func hasCompactionTrigger(raw json.RawMessage) bool {
	blocks, ok := rawJSONList(raw)
	if !ok {
		return false
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		var d struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(blocks[i], &d) == nil && d.Type == "compaction_trigger" {
			return true
		}
	}
	return false
}
