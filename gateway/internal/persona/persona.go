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
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Loader struct {
	dir   string
	log   *slog.Logger
	mu    sync.Mutex
	cache map[string]entry
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
func FileFor(sid string) string { return url.PathEscape(sid) + ".md" }

// For 返回会话使用的系统提示词，以及它来自哪个文件（都没有时返回空）。
func (l *Loader) For(sid string) (text, path string) {
	for _, name := range []string{FileFor(sid), "default.md"} {
		p := filepath.Join(l.dir, name)
		if t, ok := l.read(p); ok {
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
