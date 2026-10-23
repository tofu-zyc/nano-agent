package channel

// StatusView：简洁档的临时状态行（“正在执行 Shell…”），回答结束后由渠道删掉
type StatusView interface{ Status(string) }

// ShowView：要按档位排版的窗口（飞书卡片、Telegram 过程消息），打开时告诉它是哪一档
type ShowView interface{ SetShow(string) }
type filteredView struct {
	RunView
	show string
}

func FilterView(v RunView, show string) RunView {
	if v == nil {
		return nil
	}
	if show == "" {
		show = "过程"
	}
	if sv, ok := v.(ShowView); ok {
		sv.SetShow(show)
	}
	if show == "详细" {
		return v
	}
	f := &filteredView{v, show}
	if show == "简洁" {
		f.status("处理中")
	}
	return f
}
func (v *filteredView) status(s string) {
	if t, ok := v.RunView.(StatusView); ok {
		t.Status(s)
	}
}
func (v *filteredView) Reasoning(text string) {
	if v.show == "简洁" {
		v.status("正在思考")
	}
}
func (v *filteredView) Content(text string) {
	if v.show == "简洁" {
		v.status("")
	}
	v.RunView.Content(text)
}
func (v *filteredView) Tool(name, detail string) {
	if v.show == "简洁" {
		v.status("正在执行 " + name + "…")
	} else {
		v.RunView.Tool(name, detail)
	}
}
func (v *filteredView) ToolResult(ok bool, detail string) {
	if v.show != "简洁" {
		v.RunView.ToolResult(ok, detail)
	}
}
func (v *filteredView) Note(text string) {
	if v.show != "简洁" {
		v.RunView.Note(text)
	}
}
func (v *filteredView) Close() {
	if v.show == "简洁" {
		v.status("") // 只有简洁档有临时状态行
	}
	v.RunView.Close()
}
