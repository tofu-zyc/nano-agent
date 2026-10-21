package web

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/device"
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
	writeJSON(w, map[string]any{"connections": s.devices.List(), "ssh_hosts": device.SSHHosts(), "listener": s.devices.Listener(), "primary_listener": s.conf.Current().Kernel.Listen, "platform": runtime.GOOS, "arch": runtime.GOARCH,
		"pairing": s.devices.PairRequests(), "paired": s.devices.PairedDevices()})
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
	var req device.Connection
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
func (s *Server) devicePackage(w http.ResponseWriter, r *http.Request) {
	if !s.hasDevices(w) {
		return
	}
	gateway := r.URL.Query().Get("gateway")
	if gateway == "" {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		gateway = "tcp:" + net.JoinHostPort(host, "7700")
	}
	addr := strings.TrimPrefix(gateway, "tcp:")
	host, port, err := net.SplitHostPort(addr)
	number, parseErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || number < 1 || number > 65535 || host == "" || strings.ContainsAny(gateway, "\r\n") {
		writeErr(w, http.StatusBadRequest, "请填写网关的 IP 或域名和端口，例如 192.168.1.2:7700")
		return
	}
	data, name, err := s.devices.Package(r.URL.Query().Get("platform"), "tcp:"+addr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}
