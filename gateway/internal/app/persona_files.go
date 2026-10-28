package app

import (
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type PersonaFileInfo struct {
	Name     string   `json:"name"`
	SID      string   `json:"sid,omitempty"`
	Size     int64    `json:"size"`
	Mod      int64    `json:"mod"`
	Sessions []string `json:"sessions"`
}

func (a *App) PersonaFileText(name string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path, err := a.persona.ResolveName(name, false)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}
func (a *App) WritePersonaFile(name, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.persona.Write(name, text); err != nil {
		return err
	}
	a.publish(events.Event{T: "persona", Name: name})
	return nil
}
func (a *App) RemovePersonaFile(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.persona.Remove(name); err != nil {
		return err
	}
	a.publish(events.Event{T: "persona", Name: name})
	return nil
}
func (a *App) PersonaFiles() []PersonaFileInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := map[string]bool{}
	out := []PersonaFileInfo{}
	for _, part := range []string{"", "v1"} {
		names := []string{}
		if part == "" {
			names, _ = session.LegacyFiles(a.persona.Dir())
		} else {
			entries, _ := os.ReadDir(filepath.Join(a.persona.Dir(), part))
			for _, e := range entries {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
		}
		for _, filename := range names {
			if !strings.HasSuffix(filename, ".md") {
				continue
			}
			name, sid := filename, ""
			if name != "default.md" || part != "" {
				if part == "v1" {
					var ok bool
					sid, ok = a.store.SIDForKey(strings.TrimSuffix(name, ".md"))
					if !ok {
						continue
					}
				} else {
					var err error
					sid, err = url.PathUnescape(strings.TrimSuffix(name, ".md"))
					if err != nil {
						continue
					}
				}
				if ids[sid] {
					continue
				}
				ids[sid] = true
				name = filepath.ToSlash(persona.FileFor(sid))
			}
			path, err := a.persona.ResolveName(name, false)
			if err != nil {
				continue
			}
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			item := PersonaFileInfo{Name: name, SID: sid, Size: st.Size(), Mod: st.ModTime().UnixMilli(), Sessions: []string{}}
			if sid != "" && a.hasSession(sid) {
				item.Sessions = append(item.Sessions, sid)
			}
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == "default.md" {
			return true
		}
		if out[j].Name == "default.md" {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
}
