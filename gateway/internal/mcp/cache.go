package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 上次列到的工具存在 data/mcp-tools.json（服务名 → 工具列表）。
//
// 工具定义是前缀缓存的一部分：网关重启以后服务要过一会儿才连上（npx 要起、别的机器的内核要先连上来），
// 这段时间要是拿不到工具，会话的工具快照就会先少一截、连上后再变回来，白白丢两次缓存。
// 所以连上过的服务，重启后先用磁盘上的那份，连上了再按实际的刷新。

func (m *Manager) cachePath() string {
	if m.store == nil || m.store.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.store.path), "mcp-tools.json")
}

// loadCache 在 NewManager 里调一次（这时还没有别的协程）。
func (m *Manager) loadCache() {
	path := m.cachePath()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var all map[string][]*sdkmcp.Tool
	if err := json.Unmarshal(b, &all); err != nil {
		m.log.Warn("读 MCP 工具缓存失败，忽略", "err", err)
		return
	}
	for name, tools := range all {
		m.conns[name] = &conn{tools: tools}
	}
}

// saveCache 把现在每个服务的工具列表写回磁盘。不拿着任何 conn 的锁调用。
func (m *Manager) saveCache() {
	path := m.cachePath()
	if path == "" {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	conns := make(map[string]*conn, len(m.conns))
	for name, c := range m.conns {
		conns[name] = c
	}
	m.mu.Unlock()
	all := map[string][]*sdkmcp.Tool{}
	for name, c := range conns {
		c.mu.Lock()
		if c.tools != nil {
			all[name] = c.tools
		}
		c.mu.Unlock()
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		m.log.Warn("保存 MCP 工具缓存失败", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		m.log.Warn("保存 MCP 工具缓存失败", "err", err)
	}
}
