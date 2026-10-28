package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// A lease owns open descriptors at fixed byte boundaries and a detached settings
// value. App can release its lock before hashing/copying. History rewrites publish
// replacements; removing/renaming an old path cannot change these descriptors.
type SnapshotLease struct {
	root     string
	sid      string
	settings Settings
	parts    []snapshotPart
}
type snapshotPart struct {
	file  *os.File
	bytes int64
	lines int
}

func (s *Store) FreezeSnapshot(sid string, st Settings) (*SnapshotLease, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var frozen Settings
	if err = json.Unmarshal(b, &frozen); err != nil {
		return nil, err
	}
	l := &SnapshotLease{root: filepath.Join(s.dir, "session-snapshots"), sid: sid, settings: frozen}
	add := func(path string, lines int, optional bool) error {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) && optional {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("快照来源必须是普通文件：%s", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err = f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		l.parts = append(l.parts, snapshotPart{file: f, bytes: info.Size(), lines: lines})
		return nil
	}
	if br, ok := s.Branch(sid); ok && br.SnapshotID != "" {
		limit := br.InheritedEnd
		if !br.InheritedEndSet {
			if info, ok := s.Snapshot(br.SnapshotID); ok {
				limit = info.Messages
			} else {
				return nil, errors.New("分支快照元信息缺失")
			}
		}
		if err := add(filepath.Join(s.snapshotDir(br.SnapshotID), "messages.jsonl"), limit, false); err != nil {
			l.Close()
			return nil, err
		}
	}
	p, _, err := s.locate(sid, ".jsonl")
	if err == nil {
		err = add(p, -1, true)
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *SnapshotLease) Close() {
	for _, p := range l.parts {
		_ = p.file.Close()
	}
	l.parts = nil
}

// Atomic prefix replacement preserves already-frozen readers, unlike Truncate.
func replaceHistoryPrefix(path string, bytes int64) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".history-prefix-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err = out.Chmod(st.Mode().Perm()); err == nil {
		_, err = io.CopyN(out, in, bytes)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), path)
}

// The inherited part may end before the immutable source snapshot's physical
// end after a branch undo. It is clipped without scanning under App.mu.
type prefixLines struct {
	reader io.Reader
	left   int
	done   bool
}

func (r *prefixLines) Read(b []byte) (int, error) {
	if r.done || r.left == 0 {
		return 0, io.EOF
	}
	n, err := r.reader.Read(b)
	if r.left < 0 {
		return n, err
	}
	for i, c := range b[:n] {
		if c == '\n' {
			r.left--
			if r.left == 0 {
				r.done = true
				return i + 1, nil
			}
		}
	}
	return n, err
}
func (l *SnapshotLease) reader() io.Reader {
	readers := make([]io.Reader, 0, len(l.parts))
	for _, p := range l.parts {
		readers = append(readers, &prefixLines{reader: io.NewSectionReader(p.file, 0, p.bytes), left: p.lines})
	}
	return io.MultiReader(readers...)
}

func frozenSnapshotSettings(st Settings) SnapshotSettings {
	return SnapshotSettings{Persona: st.SnapshotPersona, Agent: st.Agent, Model: st.Model, Thinking: st.Thinking, Mode: st.Mode, Workspaces: append([]string(nil), st.Workspaces...), WorkspacesSet: st.WorkspacesSet, Title: st.Title, Group: st.Group}
}

func (l *SnapshotLease) Fingerprint() (string, error) {
	context := struct {
		Source     string
		Base       int
		Generation int64
		Summary    string
		Omitted    map[int]string
		Settings   SnapshotSettings
		Segments   []AgentSegment
	}{l.sid, l.settings.Base, l.settings.SessionGeneration, l.settings.Summary, l.settings.Omitted, frozenSnapshotSettings(l.settings), l.settings.AgentSegments}
	b, err := json.Marshal(context)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write(b)
	_, _ = h.Write([]byte{'\n'})
	if _, err = io.CopyBuffer(h, l.reader(), make([]byte, 64<<10)); err != nil {
		return "", err
	}
	return "snap-" + hex.EncodeToString(h.Sum(nil)), nil
}

func readSnapshotInfo(root, id string) (SnapshotInfo, bool) {
	b, err := os.ReadFile(filepath.Join(root, id, "context.json"))
	if err != nil {
		return SnapshotInfo{}, false
	}
	var info SnapshotInfo
	if json.Unmarshal(b, &info) != nil || info.SnapshotID != id {
		return SnapshotInfo{}, false
	}
	return info, true
}

// Write publishes a complete immutable directory. No Store maps are accessed;
// this is safe to call after releasing the App lock.
func (l *SnapshotLease) Write() (SnapshotInfo, error) {
	id, err := l.Fingerprint()
	if err != nil {
		return SnapshotInfo{}, err
	}
	if info, ok := readSnapshotInfo(l.root, id); ok {
		return info, nil
	}
	if err = os.MkdirAll(l.root, 0700); err != nil {
		return SnapshotInfo{}, err
	}
	dir, err := os.MkdirTemp(l.root, ".preparing-")
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return SnapshotInfo{}, err
	}
	count := &lineCounter{}
	n, err := io.CopyBuffer(io.MultiWriter(f, count), l.reader(), make([]byte, 64<<10))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return SnapshotInfo{}, err
	}
	info := SnapshotInfo{SnapshotID: id, SourceSID: l.sid, Messages: count.n, Base: l.settings.Base, Effective: max(0, count.n-l.settings.Base), Summary: l.settings.Summary, Omitted: l.settings.Omitted, Generation: l.settings.SessionGeneration, CreatedAt: time.Now().Unix(), Bytes: n, Settings: frozenSnapshotSettings(l.settings)}
	info.Stats, err = scanHistoryStats(l.reader())
	if err != nil {
		return SnapshotInfo{}, err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return SnapshotInfo{}, err
	}
	if err = writeFileAtomic(filepath.Join(dir, "context.json"), b); err != nil {
		return SnapshotInfo{}, err
	}
	if err = os.Rename(dir, filepath.Join(l.root, id)); err != nil {
		if existing, ok := readSnapshotInfo(l.root, id); ok {
			return existing, nil
		}
		return SnapshotInfo{}, err
	}
	return info, nil
}
