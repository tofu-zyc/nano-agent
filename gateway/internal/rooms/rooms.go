// Package rooms 保存网关内部的群聊（"房间"）：哪些 agent 在里面、点名还是自由、有没有暂停。
//
// 每个群的聊天记录就是网关里的一个会话（room:<id>），网页的会话页就能看、能发言；
// 每个成员 agent 在这个群里还有自己的会话（@<agent>/room:<id>），它在那里"听"和说话。
package rooms

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// 两种说话方式：点名（默认，只有被叫到才说）和自由（可以按积极度插话）。
const (
	ModeMention = "mention"
	ModeFree    = "free"
)

// Room 是一个内部群聊。
type Room struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"` // agent ID
	Mode    string   `json:"mode"`    // mention | free
	Paused  bool     `json:"paused,omitempty"`
	Muted   []string `json:"muted,omitempty"`  // 被禁言的 agent
	DM      []string `json:"dm,omitempty"`     // 两个 agent 的私聊：成员就是这两个
	Budget  int      `json:"budget,omitempty"` // 一天最多用多少 token（0 表示不限），超了就暂停
	Created int64    `json:"created,omitempty"`
}

// IsDM 是不是两个 agent 之间的私聊。
func (r Room) IsDM() bool { return len(r.DM) == 2 }

// Store 是 data/rooms.json。
type Store struct {
	path  string
	rooms []Room
}

// Open 读入群聊列表；文件不存在时是空的。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.rooms); err != nil {
		return nil, fmt.Errorf("读取 rooms.json：%w", err)
	}
	return s, nil
}

// List 是所有群聊（副本）。
func (s *Store) List() []Room { return slices.Clone(s.rooms) }

// Get 按 ID 找。
func (s *Store) Get(id string) (Room, bool) {
	for _, r := range s.rooms {
		if r.ID == id {
			return r, true
		}
	}
	return Room{}, false
}

// FindByName 按名字找（私聊的名字是 "agent1+agent2"）。
func (s *Store) FindByName(name string) (Room, bool) {
	for _, r := range s.rooms {
		if r.Name == name {
			return r, true
		}
	}
	return Room{}, false
}

var idOK = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Create 新建一个群聊。名字重复时自动加编号。
func (s *Store) Create(r Room) (Room, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return Room{}, errors.New("群名字不能为空")
	}
	r.Members = normalizeMembers(r.Members)
	if len(r.Members) == 0 {
		return Room{}, errors.New("至少选一个 agent 进群")
	}
	if r.Mode == "" {
		r.Mode = ModeMention
	}
	if r.Mode != ModeMention && r.Mode != ModeFree {
		return Room{}, errors.New("说话方式只能是 mention（点名）或 free（自由）")
	}
	if r.ID == "" {
		r.ID = newID()
	}
	if !idOK.MatchString(r.ID) {
		r.ID = newID()
	}
	if _, ok := s.Get(r.ID); ok {
		r.ID = newID()
	}
	base := r.Name
	for i := 2; ; i++ {
		if _, ok := s.FindByName(r.Name); !ok {
			break
		}
		r.Name = fmt.Sprintf("%s %d", base, i)
	}
	if r.Created == 0 {
		r.Created = time.Now().Unix()
	}
	s.rooms = append(s.rooms, r)
	return r, s.save()
}

// Update 改一个群聊（ID 不能改）。
func (s *Store) Update(r Room) error {
	for i := range s.rooms {
		if s.rooms[i].ID != r.ID {
			continue
		}
		if r.Name == "" {
			r.Name = s.rooms[i].Name
		}
		if r.Mode != ModeMention && r.Mode != ModeFree {
			r.Mode = s.rooms[i].Mode
		}
		r.Members = normalizeMembers(r.Members)
		if len(r.Members) == 0 {
			return errors.New("群里至少要有一个人")
		}
		r.Created = s.rooms[i].Created
		s.rooms[i] = r
		return s.save()
	}
	return errors.New("没有这个群")
}

// Delete 删掉一个群聊。
func (s *Store) Delete(id string) error {
	for i := range s.rooms {
		if s.rooms[i].ID == id {
			s.rooms = append(s.rooms[:i], s.rooms[i+1:]...)
			return s.save()
		}
	}
	return errors.New("没有这个群")
}

// Muted 判断某个 agent 在这个群里被禁言了。
func (r Room) MutedAgent(id string) bool { return slices.Contains(r.Muted, id) }

func normalizeMembers(in []string) []string {
	var out []string
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// newID 起一个随机 ID（短、不会撞）。
func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) save() error {
	if s.rooms == nil {
		s.rooms = []Room{}
	}
	b, err := json.MarshalIndent(s.rooms, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
