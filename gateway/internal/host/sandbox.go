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

func checkHookInput(req []byte, supported bool) error {
	var v map[string]json.RawMessage
	if json.Unmarshal(req, &v) != nil {
		return fmt.Errorf("执行请求不是有效 JSON")
	}
	if input, ok := v["hook_input"]; ok {
		var text *string
		if !supported {
			return fmt.Errorf("目标执行器尚不支持 Hook 事件输入，未执行，请更新执行器")
		}
		if json.Unmarshal(input, &text) != nil || text == nil || len(*text) > 4096 {
			return fmt.Errorf("Hook 输入必须为不超过 4096 字节的字符串")
		}
	}
	return nil
}
