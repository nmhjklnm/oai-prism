package facade

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoteCompaction_Detect(t *testing.T) {
	if !hasCompactionTrigger(json.RawMessage(`[{"type":"message","role":"user","content":"hi"},{"type":"compaction_trigger"}]`)) {
		t.Fatal("应认出 compaction_trigger")
	}
	if hasCompactionTrigger(json.RawMessage(`[{"type":"message","role":"user","content":"compaction_trigger"}]`)) {
		t.Fatal("正文里出现这个词不算")
	}
}

// 网关编码的 encrypted_content 能解回摘要；别的来源解不开时给出说明而不是乱码。
func TestRemoteCompaction_EncodeDecode(t *testing.T) {
	var item struct {
		Type string `json:"type"`
		Enc  string `json:"encrypted_content"`
	}
	if err := json.Unmarshal([]byte(compactionItemJSON("  进度：建好 a.txt；暗号 PINEAPPLE-42  ")), &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "compaction" {
		t.Fatalf("type = %q", item.Type)
	}
	if s, ok := decodeCompaction(item.Enc); !ok || s != "进度：建好 a.txt；暗号 PINEAPPLE-42" {
		t.Fatalf("解码 = %q %v", s, ok)
	}
	if got := compactionHistoryText("gAAAAAB-openai-ciphertext"); got != compactionUnreadable {
		t.Fatalf("外来密文应给说明: %q", got)
	}
}

// 桥把触发条目换成摘要提示词，把带回来的 compaction 条目换成摘要正文。
func TestRemoteCompaction_BridgeInput(t *testing.T) {
	trig := bridgeInputItems(json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"暗号是 PINEAPPLE-42"}]},
		{"type":"compaction_trigger"}]`), "", nil, "")
	if got := itemText(trig[len(trig)-1]); got != compactionTriggerPrompt {
		t.Fatalf("最后一条应是摘要提示词: %q", got)
	}

	enc, _ := json.Marshal(encodeCompaction("暗号 PINEAPPLE-42；下一步跑 echo step3"))
	after := bridgeInputItems(json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"暗号是 PINEAPPLE-42"}]},
		{"type":"compaction","encrypted_content":`+string(enc)+`},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]`), "", nil, "")
	var found bool
	for _, it := range after {
		if it.Role == "user" && strings.Contains(itemText(it), "下一步跑 echo step3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应把摘要解回历史: %+v", after)
	}
}

// 远端压缩一来一回后，原生续接认出摘要、接回同一个上游会话，只发新消息。
func TestRemoteCompaction_RebasesNativeConversation(t *testing.T) {
	summary := "Handoff: user gave code PINEAPPLE-42; ran echo step1 and step2; next run echo step3. " + strings.Repeat("detail ", 20)
	comp := &nativeTurn{strong: true, compaction: true, conv: nativeConv("SYS", compactionTriggerPrompt,
		historyEntry{"User", "暗号是 PINEAPPLE-42"}, historyEntry{"Assistant", "call"}, historyEntry{"User", "[CLIENT RESULT] step2"})}
	b := committed(comp, summary)

	after := &nativeTurn{strong: true, conv: nativeConv("SYS", "暗号是什么？",
		historyEntry{"User", "暗号是 PINEAPPLE-42"},
		historyEntry{"User", compactionHistoryText(encodeCompaction(summary))})}
	rest, ok := after.delta(b, after.conv.entries())
	if !ok || len(rest) != 1 || rest[0].text != "暗号是什么？" {
		t.Fatalf("应从摘要之后接上: ok=%v %+v", ok, rest)
	}
}
