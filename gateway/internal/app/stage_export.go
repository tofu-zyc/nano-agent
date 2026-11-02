package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"nano-agent/gateway/internal/rooms"
)

// 导出（设计 §14.3）：把一场戏的场景记录排成字幕（SRT）、剧本（Markdown）或原始记录（JSON）。
// 时间不用真实时间（里面含有等模型的时间），字幕按阅读速度排。

// ExportOpts 是导出的参数。From、To 是句子序号（群记录里的位置），0 表示不限。
type ExportOpts struct {
	Format   string // srt | md | json
	Inner    bool   // 带上心里话
	Channels bool   // 带上私下频道里的话和 cue
	From, To int
}

// 字幕的排法。
const (
	srtSplitOver = 40   // 超过这么多字的句子按句号、问号、感叹号拆开
	srtRunesPerS = 5    // 每条时长 = 字数 ÷ 5 秒
	srtMinMS     = 1500 // 最短 1.5 秒
	srtMaxMS     = 8000 // 最长 8 秒
	srtGapMS     = 300  // 条与条之间留 0.3 秒
)

// docItem 是正文里的一项。
type docItem struct {
	Kind    string // say | narrate | cue | system | inner
	Who     string // 说话的人（旁白、系统为空；inner 是想这句话的成员）
	To      string // cue 的对象
	Channel string // 私下频道的名字；公开为空
	Text    string
	Line    int
}

// stageDoc 是排版前整理好的一场戏。
type stageDoc struct {
	Name   string
	Brief  string
	Roles  []roleRow
	Items  []docItem
	Rounds int
	Tokens int
}

type roleRow struct{ Name, Prompt string }

func (o ExportOpts) inRange(line int) bool {
	return (o.From == 0 || line >= o.From) && (o.To == 0 || line <= o.To)
}

// buildDoc 把场景记录整理成正文：按序排，心里话放在它那句话前面，不在范围内的丢掉。
func buildDoc(room rooms.Room, recs []StageRec, name func(string) string, o ExportOpts) []docItem {
	innerOf := map[int]StageRec{}
	for _, r := range recs {
		if r.Kind == "run" && r.Said > 0 && r.Inner != "" {
			innerOf[r.Said] = r
		}
	}
	chanName := func(id string) string {
		if id == "" {
			return ""
		}
		if c, ok := room.ChannelByID(id); ok {
			return c.Name
		}
		return id
	}
	var items []docItem
	cur := 0
	for _, r := range recs {
		switch r.Kind {
		case "line":
			cur = r.Line
			if !o.inRange(cur) {
				continue
			}
			ch := chanName(r.Channel)
			if in, ok := innerOf[r.Line]; ok && o.Inner {
				items = append(items, docItem{Kind: "inner", Who: name(in.Member), Channel: ch, Text: in.Inner, Line: r.Line})
			}
			it := docItem{Kind: r.Say, Channel: ch, Text: r.Text, Line: r.Line}
			switch r.Say {
			case "say":
				it.Who = r.Name
			case "cue":
				it.To = name(r.To)
			case "system":
				it.Text = strings.TrimPrefix(r.Text, "[系统] ")
			}
			items = append(items, it)
		case "run":
			// 想了但没说话的一轮：心里话排在它发生的位置
			if r.Said == 0 && r.Inner != "" && o.Inner && o.inRange(cur) {
				items = append(items, docItem{Kind: "inner", Who: name(r.Member), Text: r.Inner, Line: cur})
			}
		}
	}
	return items
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// splitSentences 按句号、问号、感叹号拆句，标点留在句子后面。
func splitSentences(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if t := strings.TrimSpace(string(cur)); t != "" {
			out = append(out, t)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		cur = append(cur, r)
		if strings.ContainsRune("。！？!?", r) {
			flush()
		}
	}
	flush()
	return out
}

func srtTime(ms int) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// renderSRT 排字幕：人和 agent 的公开发言、旁白各占一条；频道里的话和心里话按开关；cue 和系统提示不进字幕。
func renderSRT(items []docItem, o ExportOpts) string {
	var b strings.Builder
	t, n := 0, 0
	for _, it := range items {
		if it.Kind == "cue" || it.Kind == "system" || it.Kind == "inner" && !o.Inner {
			continue
		}
		if it.Channel != "" && !o.Channels {
			continue
		}
		label, text := "", strings.Join(strings.Fields(it.Text), " ")
		switch it.Kind {
		case "inner":
			label = "（" + it.Who + "心里）"
		case "say":
			label = it.Who
			if it.Channel != "" {
				label += "（私下）"
			}
			label += "："
		case "narrate":
			if it.Channel != "" {
				label = "（私下）"
			}
		}
		pieces := []string{text}
		if runeLen(text) > srtSplitOver {
			pieces = splitSentences(text)
		}
		for i, p := range pieces {
			cue := p
			if i == 0 {
				cue = label + p
			}
			ms := min(max(runeLen(cue)*1000/srtRunesPerS, srtMinMS), srtMaxMS)
			n++
			fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", n, srtTime(t), srtTime(t+ms), cue)
			t += ms + srtGapMS
		}
	}
	return b.String()
}

// renderMD 排剧本：标题、场景说明、角色表、正文；心里话用斜体，频道和 cue 标出来，暂停写成一行说明；末尾附轮数和用量。
func renderMD(d stageDoc, o ExportOpts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", d.Name)
	if d.Brief != "" {
		fmt.Fprintf(&b, "## 场景说明\n\n%s\n\n", d.Brief)
	}
	b.WriteString("## 角色\n\n")
	for _, r := range d.Roles {
		p := r.Prompt
		if p == "" {
			p = "（没有设定角色）"
		}
		fmt.Fprintf(&b, "- **%s**：%s\n", r.Name, strings.Join(strings.Fields(p), " "))
	}
	b.WriteString("\n## 正文\n\n")
	for _, it := range d.Items {
		if (it.Channel != "" || it.Kind == "cue") && !o.Channels || it.Kind == "inner" && !o.Inner {
			continue
		}
		text := strings.TrimSpace(it.Text)
		mark := ""
		if it.Channel != "" {
			mark = "【私下·" + it.Channel + "】"
		}
		switch it.Kind {
		case "inner":
			fmt.Fprintf(&b, "*（%s心里）%s*\n\n", it.Who, strings.ReplaceAll(text, "\n", " "))
		case "say":
			fmt.Fprintf(&b, "**%s**%s：%s\n\n", it.Who, mark, text)
		case "narrate":
			fmt.Fprintf(&b, "> %s%s\n\n", mark, text)
		case "cue":
			fmt.Fprintf(&b, "**导演→%s**【cue】：%s\n\n", it.To, text)
		case "system":
			fmt.Fprintf(&b, "*（系统：%s）*\n\n", text)
		}
	}
	fmt.Fprintf(&b, "---\n\n共 %d 轮，用了 %d token。\n", d.Rounds, d.Tokens)
	return b.String()
}

// ExportStage 导出一场戏，返回内容、类型和建议的文件名。
func (a *App) ExportStage(roomID string, o ExportOpts) (data []byte, contentType, filename string, err error) {
	if !slices.Contains([]string{"srt", "md", "json"}, o.Format) {
		return nil, "", "", errors.New("format 要是 srt、md 或 json")
	}
	if o.From < 0 || o.To < 0 || (o.To != 0 && o.From > o.To) {
		return nil, "", "", errors.New("from、to 是句子序号，from 不能大于 to")
	}
	path, err := a.stageFile(roomID)
	if err != nil {
		return nil, "", "", err
	}
	var recs []StageRec
	if err := readStage(path, func(r StageRec) bool { recs = append(recs, r); return true }); err != nil {
		return nil, "", "", err
	}
	a.mu.Lock()
	room, _ := a.roomStore.Get(roomID)
	info := a.roomInfo(room)
	names := map[string]string{}
	for _, m := range room.Members {
		names[m] = a.agentDisplay(m)
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return a.agentDisplay(id)
	}
	items := buildDoc(room, recs, name, o)
	doc := stageDoc{Name: room.Name, Brief: strings.TrimSpace(room.Brief), Items: items, Rounds: info.Use.Round, Tokens: info.Use.Total}
	for _, m := range room.Members {
		doc.Roles = append(doc.Roles, roleRow{Name: names[m], Prompt: strings.TrimSpace(room.Roles[m].Prompt)})
	}
	a.mu.Unlock()
	filename = room.Name + "." + o.Format
	switch o.Format {
	case "srt":
		return []byte(renderSRT(items, o)), "application/x-subrip; charset=utf-8", filename, nil
	case "md":
		return []byte(renderMD(doc, o)), "text/markdown; charset=utf-8", filename, nil
	}
	// json：范围内的时间线记录，加上成员、规则和用量合计
	var out []StageRec
	cur := 0
	for _, r := range recs {
		if r.Kind == "line" {
			cur = r.Line
		}
		if (o.From == 0 && o.To == 0) || (cur != 0 && o.inRange(cur)) {
			if r.Kind == "prefix" {
				r.Len, r.Body = len(r.Body), ""
			}
			out = append(out, r)
		}
	}
	b, err := json.MarshalIndent(map[string]any{
		"room": map[string]any{"id": room.ID, "name": room.Name, "members": names, "rules": room.Rules},
		"use":  info.Use, "from": o.From, "to": o.To, "records": out,
	}, "", "  ")
	return b, "application/json; charset=utf-8", filename, err
}
