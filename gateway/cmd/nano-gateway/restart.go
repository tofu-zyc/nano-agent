package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
)

// restartMarker 是重启前写到 data/ 里的信息；新进程启动后据此给发起的会话回话。
type restartMarker struct {
	app.RestartRequest
}

func markerPath(dataDir string) string { return filepath.Join(dataDir, "restart.json") }

func requestRestart(cfgPath, dataDir string, req app.RestartRequest, ch chan<- app.RestartRequest) error {
	if err := config.Check(cfgPath); err != nil {
		return fmt.Errorf("配置加载不了，网关保持运行：%w", err)
	}
	if req.At == 0 {
		req.At = time.Now().UnixMilli()
	}
	b, _ := json.Marshal(restartMarker{RestartRequest: req})
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	tmp := markerPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, markerPath(dataDir)); err != nil {
		return err
	}
	select {
	case ch <- req:
		return nil
	default:
		return errors.New("已经在重启了")
	}
}

func readRestartMarker(dataDir string) (app.RestartRequest, time.Duration, bool) {
	b, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		return app.RestartRequest{}, 0, false
	}
	var m restartMarker
	if json.Unmarshal(b, &m) != nil {
		os.Remove(markerPath(dataDir))
		return app.RestartRequest{}, 0, false
	}
	elapsed := time.Since(time.UnixMilli(m.At))
	if elapsed < 0 || elapsed > 10*time.Minute {
		os.Remove(markerPath(dataDir))
		return app.RestartRequest{}, 0, false
	}
	os.Remove(markerPath(dataDir))
	return m.RestartRequest, elapsed, true
}
