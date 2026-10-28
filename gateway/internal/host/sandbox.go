package host

import (
	"encoding/json"
	"fmt"
	"slices"
)

func sandboxRequest(req []byte) string {
	var p struct {
		Mode string `json:"sandbox"`
	}
	_ = json.Unmarshal(req, &p)
	if p.Mode == "" {
		return "auto"
	}
	return p.Mode
}
func checkSandboxRequest(req []byte, modes []string) error {
	mode := sandboxRequest(req)
	if mode == "auto" {
		return nil
	}
	if (mode != "on" && mode != "off") || !slices.Contains(modes, mode) {
		return fmt.Errorf("目标执行器不支持 sandbox=%s，没有执行，请更新执行器或选择受支持的策略", mode)
	}
	return nil
}
func probeSandboxModes(line string) []string {
	var p struct {
		Modes []string `json:"sandbox_modes"`
	}
	_ = json.Unmarshal([]byte(line), &p)
	return p.Modes
}
