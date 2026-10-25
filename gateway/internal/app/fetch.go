package app

// 图片和文件（v0.7）：
//   - 看图：Read 读到图片时执行器只回一句“<路径> 是图片（PNG，N 字节）”。网关把图片从那台机器上取过来
//     （执行器的 Fetch），存进 data/uploads，附在这条工具结果上；请求模型时跟在这批工具结果后面交给它
//     （session.BuildMessages）。取图的时候，下一次请求模型要等一等。
//   - 发文件：SendFile 把工作目录里的文件或者网址上的东西发到这一轮的聊天窗口里（图片、表情包、音频、
//     视频、文档），渠道有 MediaSender 就按它的方式发，没有就发一行文字说明。和文字回复走同一个发送队列。
//
// 取文件的机器和读的范围跟这一轮的工作目录一样：网关本机、SSH 服务器、装了内核的电脑都行；
// 管理员能读整个文件系统，访客只能读挂载的工作目录。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// fetchTarget 是这一轮里某个工作目录（空表示默认的第一个）在哪台机器上，以及交给执行器的读范围。
// 没挂工作目录的管理员在网关本机的 HOME 下工作。调用方持有锁。
func (a *App) fetchTarget(r *run, workspace string) (host.Host, map[string]any, error) {
	admin := a.isAdmin(r.from)
	if len(r.rt.mounts) == 0 {
		if !admin {
			return nil, nil, errors.New("这个会话没有挂载工作目录，只能发网址上的文件")
		}
		h, _, why := a.hostFor(host.LocalName)
		if h == nil {
			return nil, nil, errors.New("网关本机用不了：" + why)
		}
		home, _ := os.UserHomeDir()
		return h, map[string]any{"dirs": []string{home}, "ro": []string{"/"}}, nil
	}
	mt := &r.rt.mounts[0]
	if workspace != "" {
		if mt = r.rt.find(workspace); mt == nil {
			return nil, nil, errors.New("这一轮没有挂载工作目录 " + workspace)
		}
	}
	var h host.Host
	if mt.local && mt.ws.Host != host.LocalName {
		h = kernelHost{a: a, node: r.node} // 内核所在的电脑
	} else {
		var why string
		if h, _, why = a.hostFor(mt.ws.Host); h == nil {
			return nil, nil, fmt.Errorf("工作目录 %s 所在的 %s 现在用不了：%s", mt.ws.Label(), mt.ws.Host, why)
		}
	}
	spec := r.rt.spec(*mt, admin)
	return h, map[string]any{"dirs": spec.Dirs, "ro": spec.RO}, nil
}

// fetchFile 让那台机器上的执行器读整个文件（最多 limit 字节）。不持有锁时调用（要等网络）。
func fetchFile(ctx context.Context, h host.Host, scope map[string]any, sid, p string, limit int) ([]byte, error) {
	req := map[string]any{"tool": "Fetch", "args": map[string]any{"path": p, "max_bytes": limit},
		"rw": []string{}, "write": false, "tmp_key": sid, "max_output": limit/3*4 + 8192, "watch_stdin": true}
	for k, v := range scope {
		req[k] = v
	}
	b, _ := json.Marshal(req)
	res := h.Exec(ctx, append(b, '\n'))
	if !res.OK {
		if strings.Contains(res.Content, "Fetch") && strings.Contains(res.Content, "未知") {
			return nil, errors.New("那台机器上的执行器太旧了，取不了文件（升级内核后就好）")
		}
		return nil, errors.New(res.Content)
	}
	return base64.StdEncoding.DecodeString(res.Content)
}

// ---- 看图 ----

var readImageRe = regexp.MustCompile(`^(.+) 是图片（(PNG|JPEG|GIF|WebP)，(\d+) 字节）$`)

// seeImage 处理 Read 读到图片的结果：把图片取过来附在工具结果上。返回 false 表示不是图片，照常处理。
// 调用方持有锁。
func (a *App) seeImage(s *sess, m *proto.Msg) bool {
	r := s.run
	call, ok := r.callArgs[m.ID]
	if !ok || call.Name != "Read" || (m.OK != nil && !*m.OK) {
		return false
	}
	match := readImageRe.FindStringSubmatch(strings.TrimSpace(m.Content))
	if match == nil {
		return false
	}
	p := match[1]
	appendResult := func(content string, imgs []media.Attachment) {
		if err := a.store.Append(s.sid, session.Msg{Role: "tool", ToolCallID: m.ID, Content: content, Images: imgs}); err != nil {
			a.log.Warn("保存工具结果失败", "sid", s.sid, "err", err)
		}
	}
	if !r.withImages || a.media == nil {
		appendResult(m.Content+"\n（现在用的模型不能看图，看不到图片内容）", nil)
		return true
	}
	var args struct {
		Workspace string `json:"workspace"`
	}
	json.Unmarshal(call.Args, &args)
	h, scope, err := a.fetchTarget(r, args.Workspace)
	if err != nil {
		appendResult(m.Content+"\n（没能把图片拿给你看："+err.Error()+"）", nil)
		return true
	}
	r.fetching++
	store, sid := a.media, s.sid
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
		data, err := fetchFile(ctx, h, scope, sid, p, media.MaxImageBytes)
		cancel()
		var att media.Attachment
		if err == nil {
			att, err = store.Save(data, filepath.Base(p), "tool")
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.log.Warn("取图片失败", "sid", sid, "path", p, "err", err)
			appendResult(m.Content+"\n（没能把图片拿给你看："+err.Error()+"）", nil)
		} else {
			appendResult(m.Content+"\n（图片附在下面，你能直接看到）", []media.Attachment{att})
			a.publish(events.Event{T: "tool_media", SID: sid, ID: m.ID, Data: map[string]any{"images": []media.Attachment{att}}})
		}
		r.fetching--
		if r.fetching == 0 && r.modelWaiting && s.run == r {
			r.modelWaiting = false
			a.callModel(s)
		}
	}()
	return true
}

// ---- 发文件 ----

func sendFileTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "SendFile", Description: "把图片、表情包、动图、音频、视频或文件发到当前聊天，对方直接收到（不是发链接）。" +
		"files 每项是工作目录里的文件路径（相对路径从工作目录开始），或者 http(s) 网址。想发表情包、图片就用它，别在回复里用文字描述图片。" +
		"要配一句话就在调用之前或之后照常回复。单个文件最多 20 MB。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"files":{"type":"array","items":{"type":"string"},"description":"要发的文件：路径或网址，按顺序发"},` +
			`"workspace":{"type":"string","description":"文件在哪个工作目录（挂了好几个时写）"}},"required":["files"]}`)}
}

// sendFileCall 执行 SendFile：取文件、存进 data/uploads、按顺序放进这一轮每个聊天窗口的发送队列。
// 调用方持有锁；取文件在后台，结束时调用 reply。
func (a *App) sendFileCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	r := s.run
	var args struct {
		Files     []string `json:"files"`
		Workspace string   `json:"workspace"`
	}
	if err := json.Unmarshal(m.Args, &args); err != nil || len(args.Files) == 0 {
		reply(false, "files 要写要发的文件（路径或网址）")
		return
	}
	if len(args.Files) > 9 {
		reply(false, "一次最多发 9 个文件")
		return
	}
	var outs []endpoint
	for _, o := range r.outs {
		if o.ep.ch != "web" && o.ep.ch != "room" {
			outs = append(outs, o.ep)
		}
	}
	if r.inner {
		reply(false, "这一轮没有聊天窗口：想发给谁，用 Message 找他")
		return
	}
	if a.media == nil {
		reply(false, "网关没有地方存文件")
		return
	}
	admin := a.isAdmin(r.from)
	var h host.Host
	var scope map[string]any
	var scopeErr error
	for _, f := range args.Files {
		if !isURL(f) {
			h, scope, scopeErr = a.fetchTarget(r, args.Workspace)
			break
		}
	}
	store, sid, proxy := a.media, s.sid, a.cfg.ProxyFor("")
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	r.execs[m.ID] = cancel
	go func() {
		defer cancel()
		var sent []media.Attachment
		var lines []string
		for _, f := range args.Files {
			var data []byte
			var err error
			name := f
			switch {
			case isURL(f):
				data, name, err = download(ctx, f, proxy, !admin)
			case scopeErr != nil:
				err = scopeErr
			default:
				data, err = fetchFile(ctx, h, scope, sid, f, media.MaxFileBytes)
				name = filepath.Base(f)
			}
			var att media.Attachment
			if err == nil {
				att, err = store.SaveFile(data, name, "agent")
			}
			if err != nil {
				lines = append(lines, fmt.Sprintf("没发出去：%s（%s）", f, err))
				continue
			}
			sent = append(sent, att)
			lines = append(lines, fmt.Sprintf("已发送%s：%s（%s）", kindLabel(att.MIME), att.Name, sizeText(att.Size)))
		}
		a.mu.Lock()
		delete(r.execs, m.ID)
		if s.run == r && len(sent) > 0 {
			r.sent[m.ID] = append(r.sent[m.ID], sent...)
			for _, att := range sent {
				data, err := store.Read(att.ID)
				if err != nil {
					continue
				}
				for _, ep := range outs {
					a.enqueue(ep, outItem{att: &att, data: data})
				}
			}
			a.publish(events.Event{T: "tool_media", SID: sid, ID: m.ID, Data: map[string]any{"sent": sent}})
		}
		a.mu.Unlock()
		if len(outs) == 0 && len(sent) > 0 {
			lines = append(lines, "（这个聊天只在网页上，文件在网页里能看到）")
		}
		reply(len(sent) > 0, strings.Join(lines, "\n"))
	}()
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// download 下载网址上的文件（最多 20 MB）。guest 为 true 时不许访问本机和内网（访客不能借它探内网）。
func download(ctx context.Context, raw, proxy string, guest bool) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, "", errors.New("网址不对")
	}
	if guest {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
		if err != nil {
			return nil, "", err
		}
		for _, ip := range ips {
			if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
				return nil, "", errors.New("不能发本机和内网的地址")
			}
		}
	}
	hc, err := httpx.NewClient(proxy, 90*time.Second)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (nano-agent)")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxFileBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > media.MaxFileBytes {
		return nil, "", fmt.Errorf("文件太大（最多 %d MB）", media.MaxFileBytes>>20)
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		name = "file"
	}
	return data, name, nil
}

func kindLabel(mt string) string {
	return map[string]string{"image": "图片", "audio": "音频", "video": "视频", "file": "文件"}[media.Kind(mt)]
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d 字节", n)
}
