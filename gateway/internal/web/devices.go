package web

import (
	"net"
	"net/http"
	"runtime"

	"nano-agent/gateway/internal/host"
)

func (s *Server) hasDevices(w http.ResponseWriter) bool {
	if s.devices == nil {
		writeErr(w, http.StatusServiceUnavailable, "设备管理未启动")
		return false
	}
	return true
}
func (s *Server) getDevices(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	writeJSON(w, map[string]any{"connections": s.devices.List(), "ssh_hosts": host.SSHHosts(), "listener": s.devices.Listener(), "primary_listener": s.conf.Current().Kernel.Listen, "platform": runtime.GOOS, "arch": runtime.GOARCH,
		"pairing": s.devices.PairRequests(), "paired": s.devices.PairedDevices(), "addrs": lanAddrs()})
}

// lanAddrs 是这台机器的局域网地址，给别的电脑填网关地址时参考（网页常常是在本机用 localhost 打开的）。
// 跳过回环网卡上的所有地址（WSL 会在 lo 上挂一个 10.255.255.254），私有网段排在前面。
func lanAddrs() []string {
	var private, other []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
				continue
			}
			if n.IP.IsPrivate() {
				private = append(private, n.IP.String())
			} else {
				other = append(other, n.IP.String())
			}
		}
	}
	return append(append([]string{}, private...), other...)
}

// decidePair 允许或拒绝一台电脑的配对请求。
func (s *Server) decidePair(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	var req struct {
		OK bool `json:"ok"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.devices.DecidePair(r.PathValue("id"), req.OK); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// unpair 移除一台配对过的电脑：口令作废，在线的话断开。
func (s *Server) unpair(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	if err := s.devices.Unpair(r.PathValue("node")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) saveDevice(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	var req host.Connection
	if !readJSON(w, r, &req) {
		return
	}
	id, err := s.devices.Save(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"id": id})
}
func (s *Server) connectDevice(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	if err := s.devices.Connect(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) disconnectDevice(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	if err := s.devices.Disconnect(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	if err := s.devices.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) deviceListener(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	old := s.conf.Current().Kernel.DeviceListen
	if err := s.devices.SetListener(req.Address); err != nil {
		writeErr(w, http.StatusBadRequest, "接入地址用不了："+err.Error())
		return
	}
	if err := s.conf.Set(map[string]any{"kernel.device_listen": req.Address}); err != nil {
		s.devices.SetListener(old)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"listener": s.devices.Listener()})
}

// ---- 工作目录列表 ----

func (s *Server) getWorkspaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.Workspaces())
}

func (s *Server) addWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host    string `json:"host"`
		Path    string `json:"path"`
		Default bool   `json:"default"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ws, err := s.app.AddWorkspace(r.Context(), req.Host, req.Path, req.Default)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, ws)
}

func (s *Server) removeWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveWorkspace(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) defaultWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.app.SetDefaultWorkspace(req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
