package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/session"
)

// 补上聊天记录
//
// 聊天平台上只看得到在那个窗口里说过的话。一个会话接着好几个窗口时，窗口切到别的会话、
// 或者在别的窗口聊了一阵再回来，就不知道之前聊了什么。这里记下每个窗口在每个会话里看到了哪里
// （历史里的位置，存在 endpoints.json），需要时把没看到的补发一段：
//
//   - 窗口切到一个会话（/session、网页里改接、刚设为管理员、第一次接上）：发最近几轮对话；
//   - 窗口说话时，会话里有它没看到的对话（别的窗口聊的）：先补上这些，再处理这条消息；
//   - /history [轮数]：随时查看最近的对话；终端连上来时也先显示最近几轮。
//
// 网页本来就能看到完整历史，不用补。

// turn 是历史里的一轮：用户说的话，和机器人最后的回复
type turn struct {
	at, via     string // 时间（10-21 15:08）和来源（命令行、飞书……），没开时间戳时为空
	user, reply string
	tools       int // 这一轮调用了几次工具
	start       int // 这一轮第一条消息在历史里的位置（算上裁剪掉的）
}

var stampRe = regexp.MustCompile(`^\[\d{4}-(\d{2}-\d{2}) 周. (\d{2}:\d{2})(?: · ([^\]\n]+))?\] `)

// turnsOf 把历史整理成一轮一轮的对话。h 的第一条在历史里的位置是 base；开头不是用户消息的部分
// （上一轮的尾巴）跳过。
func turnsOf(h []session.Msg, base int) []turn {
	var out []turn
	for i, m := range h {
		switch m.Role {
		case "user":
			t := turn{start: base + i}
			text := m.Content
			for strings.HasPrefix(text, "[系统] ") { // 环境说明不是用户说的
				_, rest, ok := strings.Cut(text, "\n")
				if !ok {
					rest = ""
				}
				text = rest
			}
			if g := stampRe.FindStringSubmatch(text); g != nil {
				t.at, t.via, text = g[1]+" "+g[2], g[3], text[len(g[0]):]
			}
			t.user = text
			out = append(out, t)
		case "assistant":
			if len(out) == 0 {
				continue
			}
			t := &out[len(out)-1]
			t.tools += len(m.ToolCalls)
			if strings.TrimSpace(m.Content) != "" {
				t.reply = m.Content
			}
		}
	}
	return out
}

func renderTurn(t turn) string {
	var b strings.Builder
	if head := strings.Trim(t.at+" · "+t.via, " ·"); head != "" {
		b.WriteString("〔" + head + "〕\n")
	}
	b.WriteString("你：" + clip(strings.TrimSpace(t.user), 200))
	reply := strings.TrimSpace(t.reply)
	if reply == "" {
		reply = "（没有回复）"
	}
	b.WriteString("\n机器人：" + clip(reply, 400))
	if t.tools > 0 {
		fmt.Fprintf(&b, "（调用了 %d 次工具）", t.tools)
	}
	return b.String()
}

// renderTurns 把几轮对话写成一段文字：从最近的往前取，整段不超过 limit 个字（至少一轮）。
// 返回文字和实际写进去的轮数。
func renderTurns(ts []turn, limit int) (string, int) {
	var parts []string
	total := 0
	for i := len(ts) - 1; i >= 0; i-- {
		p := renderTurn(ts[i])
		n := utf8.RuneCountInString(p)
		if len(parts) > 0 && total+n > limit {
			break
		}
		parts = append([]string{p}, parts...)
		total += n
	}
	return strings.Join(parts, "\n\n"), len(parts)
}

// histEnd 是会话历史的末尾位置（算上裁剪掉的）
func (a *App) histEnd(sid string) int {
	return a.store.Settings(sid).Base + len(a.store.History(sid))
}

// turnsFrom 返回会话里从位置 from 开始的完整对话，以及看到了哪里。正在进行的那一轮还没说完，
// 不算在内，看到的位置停在它前面，下次再补。调用方持有锁。
func (a *App) turnsFrom(sid string, from int) ([]turn, int) {
	st := a.store.Settings(sid)
	h := a.store.History(sid)
	if from < st.Base {
		from = st.Base
	}
	end := st.Base + len(h)
	if from >= end {
		return nil, end
	}
	ts := turnsOf(h[from-st.Base:], from)
	if s := a.sessions[sid]; s != nil && s.run != nil && len(ts) > 0 {
		end = ts[len(ts)-1].start
		ts = ts[:len(ts)-1]
	}
	return ts, end
}

// ---- 窗口看到了哪里 ----

func (a *App) seenOf(ep endpoint, sid string) (int, bool) {
	r := a.eps[ep.key()]
	if r == nil || r.Seen == nil {
		return 0, false
	}
	n, ok := r.Seen[sid]
	return n, ok
}

// markSeen 记下窗口在会话里看到了 pos。save 为 false 时只改内存，由调用方统一保存。
func (a *App) markSeen(ep endpoint, sid string, pos int, save bool) {
	r := a.eps[ep.key()]
	if r == nil || ep.ch == "web" {
		return
	}
	if r.Seen == nil {
		r.Seen = map[string]int{}
	}
	if r.Seen[sid] == pos {
		return
	}
	r.Seen[sid] = pos
	if save {
		a.saveEndpoints()
	}
}

// notice 给窗口发一段系统说明：能区分样式的渠道（终端）用灰色显示，其他的当普通消息发。
func (a *App) notice(ep endpoint, text string) {
	if n, ok := a.chans[ep.ch].(channel.Noticer); ok {
		n.Notice(ep.chat, text)
		return
	}
	a.deliver(ep, text, "")
}

// introduce 告诉窗口它接在哪个会话上，附上最近几轮对话。调用方持有锁。
func (a *App) introduce(ep endpoint, sid, lead string) {
	ts, end := a.turnsFrom(sid, 0)
	text := lead
	if len(ts) > 0 {
		recent, n := renderTurns(ts[max(0, len(ts)-3):], 1500)
		text += fmt.Sprintf("\n\n最近 %d 轮对话（/history 看更多）：\n\n%s", n, recent)
	}
	a.notice(ep, text)
	a.markSeen(ep, sid, end, true)
}

// catchUp 在窗口说话之前，把它在这个会话里没看到的对话补上（别的窗口聊的）。调用方持有锁。
func (a *App) catchUp(ep endpoint, sid string) {
	if ep.ch == "web" {
		return
	}
	from, ok := a.seenOf(ep, sid)
	if !ok {
		// 没有记录（升级前的窗口）：从现在开始算
		a.markSeen(ep, sid, a.histEnd(sid), false)
		return
	}
	ts, end := a.turnsFrom(sid, from)
	a.markSeen(ep, sid, end, false)
	if len(ts) == 0 {
		return
	}
	text, n := renderTurns(ts, 1500)
	head := "（你不在这里的时候，这个会话里还聊了这些：）"
	if n < len(ts) {
		head = fmt.Sprintf("（你不在这里的时候，这个会话里还聊了 %d 轮，这是最近的 %d 轮，/history 看更多：）", len(ts), n)
	}
	a.notice(ep, head+"\n\n"+text)
}

const historyUsage = "用法：/history 查看最近 5 轮对话，/history <轮数> 查看更多（最多 30 轮）"

func (a *App) cmdHistory(c in, arg string) {
	n := 5
	if arg != "" {
		var err error
		if n, err = strconv.Atoi(arg); err != nil || n < 1 {
			a.answer(c, historyUsage)
			return
		}
		n = min(n, 30)
	}
	ts, end := a.turnsFrom(c.s.sid, 0)
	if len(ts) == 0 {
		a.answer(c, "这个会话还没有对话")
		return
	}
	if len(ts) > n {
		ts = ts[len(ts)-n:]
	}
	text, shown := renderTurns(ts, 3500)
	head := fmt.Sprintf("会话「%s」最近 %d 轮对话：", a.sessionInfo(c.s.sid).Title, shown)
	if st := a.store.Settings(c.s.sid); st.Base > 0 {
		head += "（更早的已经因为太长裁掉了）"
	}
	a.answer(c, head+"\n\n"+text)
	a.markSeen(c.ep, c.s.sid, end, true)
}

// cliConnected 在终端连上来时打个招呼，附上最近的对话。
func (a *App) cliConnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	ep := endpoint{ch: "cli", chat: "local"}
	sid := mainSID
	if r := a.eps[ep.key()]; r != nil {
		sid = r.Session
	}
	info := a.sessionInfo(sid)
	a.introduce(ep, sid, fmt.Sprintf("已连上网关。这个终端接在会话「%s」上（%d 条消息）。/help 查看命令，/session 切换会话，Ctrl-D 退出。",
		info.Title, info.Messages))
}
