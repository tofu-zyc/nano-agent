// Package persona 读取人格（系统提示词）文件，改完保存即生效，不用重启。
//
// 查找顺序：
//
//	<dir>/<会话ID>.md   某个会话专用，比如 personas/tg:-1001234567890.md
//	<dir>/default.md    其他所有会话
//
// 每次请求模型前都会 stat 一下文件，修改时间或大小变了就重新读。
package persona

import (
	"errors"
	"log/slog"
	"nano-agent/gateway/internal/session"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Loader struct {
	sessions *session.Store
	dir      string
	log      *slog.Logger
	mu       sync.Mutex
	cache    map[string]entry
}

type entry struct {
	mod  time.Time
	size int64
	text string
}

func New(dir string, log *slog.Logger) *Loader {
	return &Loader{dir: dir, log: log, cache: map[string]entry{}}
}

// Dir 是人格文件所在的目录。
func (l *Loader) Dir() string { return l.dir }

// FileFor 是会话专属人格文件的文件名（不一定存在）。
func FileFor(sid string) string { return filepath.Join("v1", session.Key(sid)+".md") }

func (l *Loader) BindSessions(store *session.Store) { l.sessions = store }

// ResolveName accepts canonical names and translates legacy names into the
// same published view. It never writes a caller-supplied filename on Windows.
func (l *Loader) ResolveName(name string, write bool) (string, error) {
	if name == "default.md" {
		return filepath.Join(l.dir, name), nil
	}
	var sid string
	portable := filepath.ToSlash(name)
	if strings.HasPrefix(portable, "v1/") {
		key := strings.TrimSuffix(strings.TrimPrefix(portable, "v1/"), ".md")
		if !strings.HasSuffix(portable, ".md") || strings.ContainsAny(key, "/\\") {
			return "", errors.New("人格文件名不合法")
		}
		var ok bool
		if l.sessions != nil {
			sid, ok = l.sessions.SIDForKey(key)
		} else {
			sid, ok = session.ParseKey(key)
		}
		if !ok {
			return "", errors.New("人格文件键没有有效的会话映射")
		}
	} else {
		if !strings.HasSuffix(name, ".md") || strings.ContainsAny(name, "/\\") {
			return "", errors.New("人格文件名不合法")
		}
		var err error
		sid, err = url.PathUnescape(strings.TrimSuffix(name, ".md"))
		if err != nil || sid == "" {
			return "", errors.New("人格文件名不合法")
		}
	}
	if l.sessions == nil {
		return session.AuxiliaryFile(l.dir, sid, ".md"), nil
	}
	path, exists, err := l.sessions.AuxiliaryPath("persona", l.dir, ".md", sid, write)
	if err != nil {
		return "", err
	}
	if !write && !exists {
		return "", os.ErrNotExist
	}
	return path, nil
}

// File 返回 dir 下某个文件的内容（比如某个 agent 的 persona.md），改了就重新读。
// 文件不存在时返回空。
func (l *Loader) File(name string) (string, string) {
	p := filepath.Join(l.dir, name)
	if l.sessions != nil {
		var err error
		p, err = l.ResolveName(name, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			l.log.Error("人格文件定位失败，保留现场", "name", name, "err", err)
		}
		if err != nil {
			return "", ""
		}
	}
	if t, ok := l.read(p); ok {
		return t, p
	}
	return "", ""
}

// For 返回会话使用的系统提示词，以及它来自哪个文件（都没有时返回空）。
func (l *Loader) For(sid string) (text, path string) {
	for _, name := range []string{FileFor(sid), "default.md"} {
		if t, p := l.File(name); p != "" {
			return t, p
		}
	}
	return "", ""
}

func (l *Loader) read(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.cache[path]; ok && e.mod.Equal(st.ModTime()) && e.size == st.Size() {
		return e.text, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		l.log.Warn("读取人格文件失败", "file", path, "err", err)
		return "", false
	}
	text := strings.TrimSpace(string(data))
	_, seen := l.cache[path]
	l.cache[path] = entry{mod: st.ModTime(), size: st.Size(), text: text}
	if seen {
		l.log.Info("人格文件已更新，重新加载", "file", path)
	} else {
		l.log.Info("加载人格文件", "file", path, "chars", len([]rune(text)))
	}
	return text, true
}
