package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 上游单条消息上限。
//
// 上游把 [system, user] 压成一条消息交给模型，并按 UTF-8 字节限长：2026-10-04 实测
// 合计 102,299 字节可过、104,560 字节报 "This request is too large to send"，
// 中文与英文是同一条线（中文 32k tokens 照样能过），所以不是 token 限制，也不是上下文
// 窗口：上游会话保管全部历史，窗口大得多。
//
// 超限的请求照发会怎样（同日 Codex 实测）：
//   - 上游先回 too large；同一请求重发常变成 "Error while processing conversation
//     (403)"，又被当成沙箱未就绪重试 10 次，一次就是 5 分钟；
//   - 错误码是 server_error，Codex 当断线重连（Reconnecting... 1/5、2/5…），会话就此卡死。
//
// 所以放不下的一轮先拆开补种（split.go、skills.go），历史按字节预算折叠或补种
// （compress.go、native.go）。只有拆不开时（不走原生续接的请求、拆完仍放不下）才报错。
//
// 报错不能说"上下文太长"：Codex 收到 context_length_exceeded 会把窗口记成已满
// （fill_to_context_window），下一轮一开口就自动压缩；Claude Code 收到 "prompt is too long"
// 也会压缩 —— 而单条放不下跟窗口无关，压缩救不了。所以回 invalid_prompt（Codex 原样显示、
// 结束本轮、不重试、不动窗口；见 codex-rs/codex-api/src/sse/responses_error.rs），
// Anthropic 形态回不带 "prompt is too long" 的 invalid_request_error。

// ErrMessageTooLarge 表示本轮要发的单条消息超过了上游上限，且拆不开（网关预检或上游拒绝）。
var ErrMessageTooLarge = errors.New("message_too_large")

// messageTooLargeError 携带超限的量化信息，errors.Is(err, ErrMessageTooLarge) 成立。
type messageTooLargeError struct {
	Bytes    int    // 规整后 system + user 的字节数
	Limit    int    // 生效上限（未配置时为 0）
	Tokens   int    // 同一份内容的 token 数
	Upstream string // 上游拒绝时的原文；网关预检拒绝时为空
}

func (e *messageTooLargeError) Error() string {
	if e.Upstream != "" {
		return fmt.Sprintf("这一轮要发给 Prism 的单条消息 %d 字节，被上游拒收（%s）。这是 Prism 单条消息的大小上限，不是上下文窗口满了；网关没能把它拆开发送",
			e.Bytes, e.Upstream)
	}
	return fmt.Sprintf("这一轮要发给 Prism 的单条消息 %d 字节，超过上游单条上限 %d 字节（facade.max_prompt_bytes）。这是 Prism 单条消息的大小上限，不是上下文窗口满了；网关没能把它拆开发送",
		e.Bytes, e.Limit)
}

func (e *messageTooLargeError) Is(target error) bool { return target == ErrMessageTooLarge }

// promptBytes 是条目里文本内容的 UTF-8 字节数（图片走附件，不占这条消息的长度）。
func promptBytes(items []prism.InputItem) int {
	n := 0
	for _, it := range items {
		for _, c := range it.Content {
			switch c.Type {
			case "input_image", "input_file":
			default:
				n += len(c.Text)
			}
		}
	}
	return n
}

// isUpstreamTooLarge 判断上游的失败文案是否为单条消息超限。
func isUpstreamTooLarge(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "too large to send")
}

// upstreamPromptItems 返回本轮真正发往上游的条目：规整成 [system, user]，
// 非桥请求再前置平台声明。预检与 runOnce 共用，保证量的是同一份内容。
func (r *Runner) upstreamPromptItems(req *RunRequest) []prism.InputItem {
	items := canonicalUpstreamInput(req.Input)
	if !req.Bridge && r.cfg.Facade.PlatformNotice {
		items = prependSystemText(items, platformNotice)
	}
	return items
}

// checkPromptSize 在占用账号、建项目、申请沙箱之前检查提示词大小。
func (r *Runner) checkPromptSize(req *RunRequest) error {
	limit := r.cfg.Facade.PromptByteLimit()
	if limit <= 0 || req.Native != nil {
		// 走原生续接的请求放不下时由 planNative 拆开补种（split.go），不在这里拦。
		return nil
	}
	items := r.upstreamPromptItems(req)
	if n := promptBytes(items); n > limit {
		return &messageTooLargeError{Bytes: n, Limit: limit, Tokens: countInputTokens(items)}
	}
	return nil
}

// responsesErrorCode 是 response.failed 里的 error.code。单条放不下回 invalid_prompt：
// Codex 原样显示、结束本轮、不重试，也不像 context_length_exceeded 那样把窗口记成已满。
// 其余代码一律当可重试的断线。
func responsesErrorCode(err error) string {
	if errors.Is(err, ErrMessageTooLarge) {
		return "invalid_prompt"
	}
	return ""
}

// anthropicTooLargeMessage 是 Anthropic 形态的超限文案。刻意不用 "prompt is too long"：
// Claude Code 认这句去压缩上下文，而单条放不下跟上下文窗口无关。
func anthropicTooLargeMessage(err error) string {
	return "request exceeds the upstream single-message size limit (not the context window): " + err.Error()
}

// writeMessageTooLarge 在流开始之前以 HTTP 400 回超限错误（OpenAI 形态，code 为
// invalid_prompt，见 responsesErrorCode）。err 不是超限错误时返回 false、不写任何东西。
func writeMessageTooLarge(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, ErrMessageTooLarge) {
		return false
	}
	writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", "invalid_prompt", err.Error())
	return true
}

// writeAnthropicMessageTooLarge 同 writeMessageTooLarge，但用 Anthropic 的错误形态与文案。
func writeAnthropicMessageTooLarge(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, ErrMessageTooLarge) {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "invalid_request_error", "message": anthropicTooLargeMessage(err)},
	})
	_, _ = w.Write(body)
	return true
}
