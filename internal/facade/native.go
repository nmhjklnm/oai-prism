package facade

// 原生续接：会话历史交给上游保管，每轮只发增量。
//
// 上游（Prism 沙箱里的 Codex）按会话 ID 在服务端保存完整对话 —— 真实前端就是这么用的：
// 每轮只发 [system, 本轮 user] 加 conversationId。上限没测到：2026-10-04 同一会话灌入
// 230 万 tokens 随机字母，回头问最早几段的开头字母仍逐字答对，会话本身从未报错（最后是
// 账号被限流才停）；每轮耗时不随会话变长而增加，说明上游自己管理长会话（不是每轮全文
// 喂给模型）。
// 关键是会话 ID 必须经前端的 Server Action（createProjectConversation）在服务端
// 创建：2026-10-04 Go 侧实测（internal/facade 的 liveprobe）——
//
//	登记过的 cid + 增量             → 记得暗号（只带 cid 就够，previousResponseId / 快照可有可无）
//	不带 cid（上游临时分配一个）+ 增量 → 不记得；把临时 cid 带回去也不记得
//	自造 cid                        → 403（1b1a67b）
//
// 2026-10-03 "只发增量必然失忆"的结论（prism-upstream-no-server-side-history）就是
// 没登记会话造成的。
//
// 本文件维护"客户端会话 → 上游会话"的绑定，并算出每轮该发的增量：
//
//   - 客户端（Codex、Chat 客户端）每轮仍发完整历史。按条目指纹比对绑定里已送达的
//     前缀，只把之后的新条目发给上游；对不上（换账号、改写了历史）就新建上游会话：
//     历史一条放得下就随首轮发出，放不下就先分段补种。绑定落盘（native_store.go），
//     网关重启不算对不上。
//   - 强会话键（Codex 会话 ID、X-Oaiprism-Session 等）下助手条目不参与比对：它们由
//     上游生成，客户端回传的形态（工具调用项）与上游原文不同。弱键（首条消息指纹）
//     会撞键，必须连助手原文一起比对，防止两段同开头的对话串进同一个上游会话。
//     比对方式随绑定走：弱键建的绑定之后凭回复句柄 / 会话 ID 找回时也照弱键比。
//   - 只发本轮消息的客户端（Responses 的 previous_response_id、回传会话 ID 的 Chat
//     客户端）凭句柄找到绑定，直接追加：绑定同时挂在会话链键与 "cid:<上游会话 ID>" 下。
//   - Codex 本地压缩后历史被替换成"若干条 user + 摘要"。摘要正是上游在这个会话里
//     刚写的，认出它就把绑定对齐到压缩后的历史，继续用同一个上游会话（上游那边的
//     完整记忆比摘要详细得多）。
//   - system 不重发：上游会话里已有，重复只会挤占窗口（带上客户端的技能清单、工具目录，
//     Codex 的 system 有几十 KB）。内容变了只发变了的那一段（systemUpdate），变动太大才
//     整份重发。每轮本轮消息末尾另有一句执行提醒（localExecReminder），要紧的约定始终在近处。
//   - 同一会话上一轮还没结束就来了新请求：客户端已经放弃了上一轮（用户打断、断线重发；
//     停止信号不一定经中转站传到网关）。强键下先停掉上一轮再接着发增量，不另起会话整份重发。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/prism"
)

const (
	// nativeBindingTTL 是绑定的闲置寿命。上游会话本身不随沙箱回收而丢失（见 liveprobe），
	// 绑定也落盘（native_store.go）：隔几天 resume 的 Codex 会话照样接回原来的上游会话。
	nativeBindingTTL = 7 * 24 * time.Hour
	// nativeSupersedeWait 是停掉上一轮后等它让出会话的最长时间（通知上游停止最多 5 秒）。
	nativeSupersedeWait = 15 * time.Second
	// nativeBriefSystem 是不重发完整 system 的轮次里代替它的一句话。
	nativeBriefSystem = "(The instructions given earlier in this conversation remain in force.)"
	// nativeSinceHeader 引出"上一轮回复之后客户端新增的其余条目"（并行工具结果等）。
	nativeSinceHeader = "[Messages since your last reply]\n"
	// nativeSeedMaxBytes 是新建会话时最多补种的历史字节。补种逐段串行（每段一轮约 10~20 秒），
	// 只补最近这么多，免得这一轮等太久；绑定落盘后只有换号、改写历史才需要补种。
	nativeSeedMaxBytes = 768 << 10
	nativeSeedSystem   = "Earlier parts of this conversation are being restored. Read them as context only: do not act on them, and reply with exactly OK."
	nativeSeedHeader   = "[Earlier conversation, part %d of %d - context only, reply OK]\n"
)

// nativeConversation 是一轮请求拆成的三部分：合并后的 system、往轮对话、本轮消息。
type nativeConversation struct {
	system  string
	history []historyEntry
	current prism.InputItem
	// currentText 是本轮消息到了下一轮历史里的样子（为空时取 current 的文本）：
	// 桥给本轮消息临时附加的提醒不进历史，比对时要去掉。
	currentText string
	// extra 是本轮附加的 system 指令（如 Codex 压缩指令）：每轮照发，不计入 system 指纹。
	extra string
}

// entries 返回往轮对话加本轮消息（本轮记作 User）。
func (c *nativeConversation) entries() []historyEntry {
	es := make([]historyEntry, 0, len(c.history)+1)
	es = append(es, c.history...)
	cur := c.currentText
	if cur == "" {
		cur = itemText(c.current)
	}
	return append(es, historyEntry{speaker: "User", text: cur})
}

// logicalItems 是不裁剪的完整上下文（用量按它计：上游模型每轮读的就是整段会话）。
func (c *nativeConversation) logicalItems() []prism.InputItem {
	return []prism.InputItem{prism.NewSystemItem(c.system + renderHistory(c.history, 0)), c.current}
}

// nativeTurn 是 handler 交给 runner 的原生续接请求。
type nativeTurn struct {
	key string
	// strong 表示会话键来自客户端显式标识（不会撞键）：只发本轮消息、压缩后的替换历史
	// 这类前缀对不上的情形，只在强键下才敢接续。
	strong     bool
	compaction bool
	conv       *nativeConversation
	// weak 表示绑定建在弱键上（由 planNative 从绑定取）：凭句柄找回时也要照弱键比对。
	weak bool

	plan *nativePlan // runner 在选定账号与项目后填写
	// cancel 停掉本轮（runner.Run 设置）：持锁期间登记在绑定上，同一会话的下一个请求凭它停掉本轮。
	cancel context.CancelFunc
}

// nativePlan 是本轮的执行方案，成功后由 commit 写回绑定。
type nativePlan struct {
	b         *nativeBinding // 已加锁；nil 表示本轮不碰绑定（并发撞车）
	cid       string         // 发给上游的会话 ID；空 = 不带会话 ID 发单条全量（回退）
	continued bool           // true = 续接已有会话发增量；false = 新会话发全量
	delivered []uint64
	sysHash   uint64
	system    string // 本轮之后上游会话里生效的 system 全文（下次算变化用）
	// seeds 是新建会话后、本轮之前要先发的历史补种消息；fallback 是补种失败时改发的全量条目。
	seeds    [][]prism.InputItem
	fallback []prism.InputItem
}

// nativeBinding 是一个客户端会话绑定的上游会话。
type nativeBinding struct {
	mu sync.Mutex // 一轮一锁：同一个上游会话不能并发追加
	// key 是绑定建立时的会话键（落盘主键）；aliases 是之后指向它的其他会话键
	// （会话链键、"cid:<上游会话 ID>"），受 nativeBindings.mu 保护。
	key       string
	aliases   []string
	account   string
	project   string
	cid       string
	delivered []uint64 // 上游会话已含的条目指纹（强键下不含助手条目）
	sysHash   uint64   // 上游会话里生效的 system 指纹
	system    string   // 上游会话里生效的 system 全文（内容变了时只发变化，见 systemUpdate）
	summary   string   // 最近一次 Codex 压缩请求里上游写的摘要
	weak      bool     // 绑定建在弱键上：助手条目参与比对（凭句柄找回时也照此比）
	updated   time.Time

	// inflight 停掉正在跑的这一轮（持锁的那一轮登记，见 supersede）。
	inflightMu sync.Mutex
	inflight   context.CancelFunc
}

func (b *nativeBinding) setInflight(cancel context.CancelFunc) {
	b.inflightMu.Lock()
	b.inflight = cancel
	b.inflightMu.Unlock()
}

// stopInflight 停掉正在跑的一轮；没有登记时返回 false。
func (b *nativeBinding) stopInflight() bool {
	b.inflightMu.Lock()
	cancel := b.inflight
	b.inflightMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (b *nativeBinding) reset() {
	b.account, b.project, b.cid, b.summary = "", "", "", ""
	b.delivered, b.sysHash, b.system = nil, 0, ""
}

var nativeBindings = struct {
	mu     sync.Mutex
	m      map[string]*nativeBinding
	lastGC time.Time
}{m: map[string]*nativeBinding{}}

// nativeBindingFor 取（或建）会话键对应的绑定。
func nativeBindingFor(key string) *nativeBinding {
	now := time.Now()
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	if now.Sub(nativeBindings.lastGC) > time.Minute {
		nativeBindings.lastGC = now
		for k, b := range nativeBindings.m {
			if b.mu.TryLock() {
				if now.Sub(b.updated) > nativeBindingTTL {
					delete(nativeBindings.m, k)
				}
				b.mu.Unlock()
			}
		}
	}
	b, ok := nativeBindings.m[key]
	if !ok {
		b = &nativeBinding{key: key, updated: now}
		nativeBindings.m[key] = b
	}
	return b
}

func entryFingerprint(e historyEntry) uint64 {
	h := fnv.New64a()
	h.Write([]byte(e.speaker))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(e.text)))
	return h.Sum64()
}

func textFingerprint(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// weakMatch 报告本轮是否连助手条目一起比对：弱键请求，或弱键建的绑定。
func (nt *nativeTurn) weakMatch() bool { return nt.weak || !nt.strong }

// matchable 报告条目是否参与前缀比对（强键下助手条目不参与，见文件头）。
func (nt *nativeTurn) matchable(e historyEntry) bool {
	return nt.weakMatch() || e.speaker != "Assistant"
}

// fingerprints 是本轮全部可比对条目的指纹（本轮成功后即为上游会话已含的内容）。
func (nt *nativeTurn) fingerprints(es []historyEntry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, e := range es {
		if nt.matchable(e) {
			out = append(out, entryFingerprint(e))
		}
	}
	return out
}

// fresh 去掉增量里由上游生成的条目（强键下的助手条目）。
func (nt *nativeTurn) fresh(es []historyEntry) []historyEntry {
	if nt.weakMatch() {
		return es
	}
	out := make([]historyEntry, 0, len(es))
	for _, e := range es {
		if e.speaker != "Assistant" {
			out = append(out, e)
		}
	}
	return out
}

// delta 算出上游会话还没见过的条目（最后一条必是本轮消息）。ok=false 表示对不上，须新建会话。
func (nt *nativeTurn) delta(b *nativeBinding, es []historyEntry) ([]historyEntry, bool) {
	// 1) 已送达的内容是本轮历史的前缀。
	i, j := 0, 0
	for ; i < len(es) && j < len(b.delivered); i++ {
		if !nt.matchable(es[i]) {
			continue
		}
		if entryFingerprint(es[i]) != b.delivered[j] {
			break
		}
		j++
	}
	if j == len(b.delivered) && i < len(es) {
		return nt.fresh(es[i:]), true
	}
	if !nt.strong {
		return nil, false
	}
	// 整段历史上游都已收到：同一轮上次发出后被中途停掉（打断、断线重发），上游有这条消息、
	// 没有完整回答。在原会话里重发本轮消息，不另起会话整份重发。
	if j == len(b.delivered) && j > 0 && i == len(es) {
		return es[len(es)-1:], true
	}
	// 2) 只发本轮消息的客户端（显式会话键、不带历史）：直接追加。
	if len(nt.conv.history) == 0 {
		return es, true
	}
	// 3) Codex 压缩后的替换历史：从上游写的摘要处接上。
	if len(b.summary) >= 64 {
		needle := b.summary[:min(len(b.summary), 256)]
		for k := len(es) - 1; k >= 0; k-- {
			if es[k].speaker == "User" && strings.Contains(es[k].text, needle) {
				if k == len(es)-1 {
					return es[k:], true
				}
				return nt.fresh(es[k+1:]), true
			}
		}
	}
	return nil, false
}

// deltaItems 把增量组装成上游要的 [system, user]，并返回 system 指纹与这轮 system 的发法
// （brief：一句话代替；update：只发变了的部分；full：整份重发）。
// limit 是单条提示词字节上限（0 不限），notice 非空时随完整 system 前置。
func (nt *nativeTurn) deltaItems(b *nativeBinding, rest []historyEntry, limit int, notice string) ([]prism.InputItem, uint64, string) {
	conv := nt.conv
	cur := conv.current
	cur.Content = append([]prism.InputContent(nil), cur.Content...)
	curText := itemText(cur)

	sysHash := textFingerprint(conv.system)
	system, mode := nativeBriefSystem, "brief"
	if sysHash != b.sysHash {
		if upd, ok := systemUpdate(b.system, conv.system); ok {
			system, mode = upd, "update"
		} else {
			system, mode = conv.system, "full"
			if notice != "" {
				system = notice + "\n\n" + system
			}
		}
	}
	if conv.extra != "" {
		system += "\n\n" + conv.extra
	}

	// 本轮消息之前的新条目（并行工具结果、客户端插入的消息）放在本轮消息前面，
	// 超出单条上限时按折叠历史的规则裁剪（compress.go）。
	if prior := rest[:len(rest)-1]; len(prior) > 0 {
		budget := historyBudget(limit, len(system)+len(curText)+len(nativeSinceHeader))
		if h := strings.TrimPrefix(renderHistory(prior, budget), historyHeader); h != "" {
			prefix := nativeSinceHeader + h + "\n\n"
			if len(cur.Content) > 0 && (cur.Content[0].Type == "" || cur.Content[0].Type == prism.BlockInputText) {
				cur.Content[0].Text = prefix + cur.Content[0].Text
			} else {
				cur.Content = append([]prism.InputContent{{Type: prism.BlockInputText, Text: prefix}}, cur.Content...)
			}
		}
	}
	return []prism.InputItem{prism.NewSystemItem(system), cur}, sysHash, mode
}

// itemText 拼出条目里全部文本块。
func itemText(it prism.InputItem) string {
	var sb strings.Builder
	for _, c := range it.Content {
		switch c.Type {
		case "input_image", "input_file":
		default:
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// nativeBindingPeek 取会话键已有的绑定（没有时为 nil，不新建）。
func nativeBindingPeek(key string) *nativeBinding {
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	return nativeBindings.m[key]
}

// streamConversationID 是流式响应头里能给出的会话 ID：头必须早于首帧写出，那时只知道
// 续接中的上游会话（新建的会话 ID 随结束帧 / 响应体给出）。
func streamConversationID(req *RunRequest) string {
	if req.Native == nil {
		return ""
	}
	b := nativeBindingPeek(req.Native.key)
	if b == nil || !b.mu.TryLock() {
		return ""
	}
	defer b.mu.Unlock()
	return b.cid
}

// nativeAlias 让 alias 指向 key 的绑定（同一个对象）：凭回复句柄 / 会话 ID 续接的请求
// 落在别的会话键上，也能找回原来的上游会话。返回新挂上别名的绑定（key 还没有绑定、
// 或别名已在时为 nil）。
func nativeAlias(alias, key string) *nativeBinding {
	if alias == "" || alias == key {
		return nil
	}
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	b, ok := nativeBindings.m[key]
	if !ok {
		return nil
	}
	return b.addAliasLocked(alias)
}

// addAliasLocked 把 alias 挂到绑定上（调用方持 nativeBindings.mu）。已挂过时返回 nil。
func (b *nativeBinding) addAliasLocked(alias string) *nativeBinding {
	if cur, ok := nativeBindings.m[alias]; ok && cur == b {
		return nil
	}
	nativeBindings.m[alias] = b
	if alias != b.key {
		b.aliases = append(b.aliases, alias)
		// 每轮都可能新增会话链键：只留最近的若干个，淘汰的连同表项一起删。
		// 上游会话 ID 的别名（客户端会带回来）不淘汰，一个会话也就一两个。
		for i := 0; len(b.aliases) > chainMaxAliases && i < len(b.aliases); {
			old := b.aliases[i]
			if strings.HasPrefix(old, "cid:") || strings.Contains(old, "|cid:") {
				i++
				continue
			}
			if nativeBindings.m[old] == b {
				delete(nativeBindings.m, old)
			}
			b.aliases = append(b.aliases[:i:i], b.aliases[i+1:]...)
		}
	}
	return b
}

// nativeConvKey 是上游会话 ID 对应的会话键（与 conversationKey 的 "cid:" 键同形，按租户隔离）：
// 客户端把我们回传的会话 ID 带回来时，会话键就是它。
func nativeConvKey(key, cid string) string {
	if t := tenantOfKey(key); t != "" {
		return t + "|cid:" + cid
	}
	return "cid:" + cid
}

// attachNative 给请求挂上原生续接，并沿用绑定里的项目：上游会话挂在项目下，
// 换了项目就只能新建会话。项目与会话都是账号私有的 —— 实际租到别的账号时，
// runner 按 BoundAccountID 作废这个项目（dropContinuation），planNative 随之新建会话。
func (h *Handler) attachNative(runReq *RunRequest, nt *nativeTurn) {
	runReq.Native = nt
	if runReq.ProjectID != "" {
		return
	}
	b := nativeBindingPeek(nt.key)
	if b == nil || !b.mu.TryLock() {
		return
	}
	project, account := b.project, b.account
	b.mu.Unlock()
	if project == "" {
		return
	}
	runReq.ProjectID = project
	runReq.MarkProjectFromChain()
	if runReq.BoundAccountID == "" {
		runReq.BoundAccountID = account
	}
}

// ---------------------------- runner 侧 ----------------------------

// planNative 在选定账号与项目后决定本轮怎么发：续接已有上游会话发增量，或新建会话发全量。
// full 是全量折叠后的条目（回退用）。返回本轮实际要发的条目，方案记在 req.Native.plan。
func (r *Runner) planNative(ctx context.Context, p prism.Principal, acctID, projectID string, req *RunRequest, full []prism.InputItem) []prism.InputItem {
	nt := req.Native
	nt.plan = &nativePlan{}
	if projectID == "" {
		return full // 会话挂在项目下：没有项目只能单条全量
	}
	b := nativeBindingFor(nt.key)
	if !b.mu.TryLock() && !(nt.strong && r.supersede(ctx, b, nt.key)) {
		// 停不掉上一轮（弱键会撞键，不敢停别人的）：这一轮发单条全量、不带会话 ID，不碰绑定。
		r.log.Info("原生续接：会话正忙，本轮按全量发送", "key", nt.key)
		return full
	}
	b.setInflight(nt.cancel)
	plan := nt.plan
	plan.b = b
	es := nt.conv.entries()
	// 弱键建的绑定一直按弱键比（凭句柄找回时 nt.strong 为真，但已送达的指纹里含助手条目）。
	nt.weak = b.weak

	if b.cid != "" && b.account == acctID && b.project == projectID {
		plan.delivered = nt.fingerprints(es)
		if rest, ok := nt.delta(b, es); ok {
			notice := ""
			if !req.Bridge && r.cfg.Facade.PlatformNotice {
				notice = platformNotice
			}
			items, sysHash, sysMode := nt.deltaItems(b, rest, r.cfg.Facade.PromptByteLimit(), notice)
			plan.cid, plan.continued, plan.sysHash, plan.system = b.cid, true, sysHash, nt.conv.system
			// 增量本身放不下（本轮贴了大段内容、工具结果很长）：前面几段先补种进同一个会话。
			// 补种失败没有可退的全量（会话里已有历史），fallback 留空，由 runOnce 直接报错。
			if seeds, split := splitOversize(items, r.cfg.Facade.PromptByteLimit()); len(seeds) > 0 {
				plan.seeds, items = seeds, split
			}
			r.log.Info("原生续接：发送增量", "key", nt.key, "cid", b.cid, "newEntries", len(rest),
				"seedParts", len(plan.seeds), "bytes", promptBytes(items), "system", sysMode)
			return items
		}
		r.log.Info("原生续接：客户端历史与上游会话对不上，新建会话", "key", nt.key, "oldCid", b.cid)
	}
	plan.delivered = nt.fingerprints(es)

	cid, err := r.createConversation(ctx, p, projectID)
	if err != nil {
		r.log.Warn("原生续接：创建上游会话失败，本轮发单条全量（下一轮再登记）", "project", projectID, "err", err)
		r.app.ConversationOps.Inc("create", "error")
		return full
	}
	r.app.ConversationOps.Inc("create", "ok")
	plan.cid = cid
	plan.sysHash, plan.system = textFingerprint(nt.conv.system), nt.conv.system

	// 新会话里要先补种的：移出 system 的完整技能清单（见 skills.go）、一条放不下的历史，
	// 最后是本轮仍放不下时拆出的前几段（见 split.go）。补种失败就改发 full（裁剪后的全量）。
	limit := r.cfg.Facade.PromptByteLimit()
	seeds := skillsSeedTurns(req.Skills, limit)
	skillParts, historyParts := len(seeds), 0
	items := full
	// 历史一条放不下（全量折叠时被裁过）：先分段把历史补种进新会话，再发本轮 ——
	// 换号、网关重启、旧会话作废之后，上游照样拿到客户端手里的完整历史。
	if historyTrimmed(full) {
		if hs := nt.seedTurns(limit); len(hs) > 0 {
			notice := ""
			if !req.Bridge && r.cfg.Facade.PlatformNotice {
				notice = platformNotice
			}
			seeds, historyParts = append(seeds, hs...), len(hs)
			items = nt.currentWithSystem(notice)
		}
	}
	extra, items := splitOversize(items, limit)
	seeds = append(seeds, extra...)
	if len(seeds) > 0 {
		plan.seeds, plan.fallback = seeds, full
	}
	r.log.Info("原生续接：新建上游会话", "key", nt.key, "cid", cid, "skillParts", skillParts,
		"historyParts", historyParts, "splitParts", len(extra), "bytes", promptBytes(items))
	return items
}

// supersede 停掉同一会话正在跑的上一轮，等它让出会话；拿到锁返回 true。
//
// 同一会话的请求是一问一答：上一轮没结束就来了新请求，说明客户端已经放弃了上一轮
// （用户打断、断线重发）。停止信号不一定传得到网关 —— 2026-10-05 实测经中转站时，
// 用户打断后那一轮在上游又跑了 10 秒、照样计费，新消息只能另起会话整份重发。
func (r *Runner) supersede(ctx context.Context, b *nativeBinding, key string) bool {
	if b.stopInflight() {
		r.log.Info("原生续接：同一会话来了新请求，停掉上一轮", "key", key)
	}
	deadline := time.Now().Add(nativeSupersedeWait)
	for {
		if b.mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) || sleepCtx(ctx, 100*time.Millisecond) != nil {
			return false
		}
	}
}

// awaitSession 在选号之前等会话空出来：上一轮还占着就先停掉它。
//
// 必须在选号之前：上一轮还占着会话绑定的号的并发槽，粘性选号会一直等那个槽；而且
// handler 挂续接时会话正忙、没拿到绑定的项目与号（见 attachNative），这里补上 ——
// 否则这一轮可能落到别的号上，只能新建会话整份重发。
func (r *Runner) awaitSession(ctx context.Context, nt *nativeTurn, req *RunRequest) {
	b := nativeBindingPeek(nt.key)
	if b == nil || (!b.mu.TryLock() && !r.supersede(ctx, b, nt.key)) {
		return
	}
	project, account := b.project, b.account
	b.mu.Unlock()
	if req.ProjectID == "" && project != "" {
		req.ProjectID = project
		req.MarkProjectFromChain()
		if req.BoundAccountID == "" {
			req.BoundAccountID = account
		}
	}
}

// currentWithSystem 是 [完整 system, 本轮消息]（补种之后的那一轮用）。
func (nt *nativeTurn) currentWithSystem(notice string) []prism.InputItem {
	system := nt.conv.system
	if notice != "" {
		system = notice + "\n\n" + system
	}
	if nt.conv.extra != "" {
		system += "\n\n" + nt.conv.extra
	}
	return []prism.InputItem{prism.NewSystemItem(system), nt.conv.current}
}

// seedTurns 把往轮对话切成若干段补种消息，每段不超过单条上限；只补最近的
// nativeSeedMaxBytes 字节（再早的上游窗口也装不下，会被它自己压缩掉）。limit <= 0 时不补种。
func (nt *nativeTurn) seedTurns(limit int) [][]prism.InputItem {
	budget := limit - len(nativeSeedSystem) - 160 - promptOverheadReserve
	if limit <= 0 || budget < 4096 {
		return nil
	}
	var lines []string
	for _, e := range nt.conv.history {
		t := strings.TrimSpace(e.text)
		if t == "" {
			continue
		}
		l := e.speaker + ": " + t
		if len(l) > budget {
			l = cutUTF8(l, budget-32) + " …[truncated]"
		}
		lines = append(lines, l)
	}
	start, total := len(lines), 0
	for start > 0 && total+len(lines[start-1])+1 <= nativeSeedMaxBytes {
		start--
		total += len(lines[start]) + 1
	}
	omitted := start
	lines = lines[start:]

	var chunks [][]string
	var cur []string
	size := 0
	for _, l := range lines {
		if size+len(l)+1 > budget && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += len(l) + 1
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	out := make([][]prism.InputItem, 0, len(chunks))
	for k, c := range chunks {
		head := fmt.Sprintf(nativeSeedHeader, k+1, len(chunks))
		if k == 0 && omitted > 0 {
			head += fmt.Sprintf("[%d older messages omitted]\n", omitted)
		}
		out = append(out, []prism.InputItem{prism.NewSystemItem(nativeSeedSystem), prism.NewUserItem(head + strings.Join(c, "\n"))})
	}
	return out
}

// cutUTF8 把 s 截到不超过 n 字节，且不切开多字节字符。
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// seedConversation 依次发送补种消息（每段一轮，等上游答完再发下一段）。
func (r *Runner) seedConversation(ctx context.Context, acct *account.Account, req *RunRequest, projectID string) error {
	plan := req.Native.plan
	for i, seed := range plan.seeds {
		sreq := &RunRequest{
			Seed: true, Input: seed, Model: req.Model, Effort: req.Effort, UserID: req.UserID,
			ConversationID: plan.cid, ProjectID: projectID, StickyKey: req.StickyKey,
			API: req.API, Bridge: true, ExtraHeaders: req.ExtraHeaders, Deadline: req.Deadline,
		}
		if _, err := r.runOnce(ctx, acct, sreq, nil); err != nil {
			return fmt.Errorf("补种第 %d/%d 段: %w", i+1, len(plan.seeds), err)
		}
	}
	return nil
}

// textOfSystem 返回规整条目里 system 的文本（没有 system 时为空）。
func textOfSystem(items []prism.InputItem) string {
	if len(items) == 0 || !isSystemRole(items[0].Role) {
		return ""
	}
	return itemText(items[0])
}

// createConversation 经 Server Action 在项目下登记一个上游会话。
func (r *Runner) createConversation(ctx context.Context, p prism.Principal, projectID string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		raw, err := r.client.ServerAction(ctx, p, prism.ActionCreateProjectConversation, projectID)
		if err == nil {
			var cid string
			if json.Unmarshal(raw, &cid) == nil && strings.HasPrefix(cid, "cdx") {
				return cid, nil
			}
			return "", errors.New("createProjectConversation 返回值不是会话 ID: " + truncateRunes(string(raw), 120))
		}
		lastErr = err
		if !isSentinelThrottle(err) {
			break
		}
		if serr := sleepCtx(ctx, time.Duration(attempt)*time.Second); serr != nil {
			return "", serr
		}
	}
	return "", lastErr
}

// commit 把成功的一轮写回绑定（并落盘）后解锁。r 为 nil 时不落盘。
// complete=false 是发出后被中途停掉的一轮（见 runner.Run）：上游已收到本轮内容、没有完整
// 回答，只记送达的条目。
func (nt *nativeTurn) commit(res *RunResult, r *Runner, complete bool) {
	plan := nt.plan
	if plan == nil || plan.b == nil {
		return
	}
	b := plan.b
	plan.b = nil
	b.setInflight(nil)
	defer b.mu.Unlock()
	defer r.persistNative(b)
	if plan.cid == "" || res == nil {
		b.reset()
		return
	}
	newConv := b.cid != plan.cid
	b.account, b.project, b.cid, b.weak = res.AccountID, res.ProjectID, plan.cid, nt.weakMatch()
	b.delivered = plan.delivered
	if nt.weakMatch() && complete {
		b.delivered = append(b.delivered, entryFingerprint(historyEntry{speaker: "Assistant", text: res.Text}))
	}
	if newConv {
		// 回传给客户端的会话 ID 也能找回这个绑定（见 nativeConvKey）。
		nativeBindings.mu.Lock()
		b.addAliasLocked(nativeConvKey(nt.key, plan.cid))
		nativeBindings.mu.Unlock()
	}
	b.sysHash, b.system = plan.sysHash, plan.system
	if nt.compaction && complete {
		b.summary = strings.TrimSpace(res.Text)
	}
	b.updated = time.Now()
}

// release 在失败时解锁绑定；drop 为 true 时作废它（上游会话已不可用，连同落盘的记录）。
func (nt *nativeTurn) release(drop bool, r *Runner) {
	plan := nt.plan
	if plan == nil || plan.b == nil {
		return
	}
	b := plan.b
	plan.b = nil
	b.setInflight(nil)
	if drop {
		b.reset()
		r.persistNative(b)
	}
	b.mu.Unlock()
}

// isConversationGone 判断续接失败是否意味着上游会话本身已不可用（须换新会话重来）。
//
// 比 isContinuationError 严：上游的瞬时失败文案多含 "conversation"（如
// "Error while processing conversation (403)"），据此作废绑定会白白丢掉整段记忆。
func isConversationGone(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrPollTimeout) || errors.Is(err, ErrMessageTooLarge) {
		return false
	}
	var ae *creds.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"conversation_too_large", "conversation too large", "not found", "no longer", "expired"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}
