package web

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"nano-agent/gateway/internal/media"
)

// uploadImage 接收网页粘贴、拖拽或者选择的一张图片，返回它的引用（ID）。
func (s *Server) uploadImage(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeErr(w, http.StatusServiceUnavailable, "图片存储没有启用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxImageBytes+1<<20)
	if err := r.ParseMultipartForm(media.MaxImageBytes + 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("图片太大（最多 %d MB）", media.MaxImageBytes>>20))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "没有收到图片")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, media.MaxImageBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读图片失败："+err.Error())
		return
	}
	att, err := s.media.Save(data, filepath.Base(hdr.Filename), "web")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, att)
}

// serveImage 按 ID 返回一张图片（对话里显示历史图片用，要登录）。
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeErr(w, http.StatusNotFound, "没有这张图片")
		return
	}
	b, err := s.media.Read(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "没有这张图片")
		return
	}
	h := w.Header()
	h.Set("Content-Type", http.DetectContentType(b))
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}
