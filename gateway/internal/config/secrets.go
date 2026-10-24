package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"nano-agent/gateway/internal/secret"
)

// secretKeys 是我们自己保存、需要加密的字段。llm.api_key 是旧版单模型配置，
// 只用于升级时导入 models.json；它也会被搬进 config.json 加密，免得留在 gateway.toml 里。
var secretKeys = []string{"web.password", "kernel.token", "telegram.token", "feishu.app_secret", "llm.api_key"}

func isSecretKey(key string) bool {
	for _, k := range secretKeys {
		if k == key {
			return true
		}
	}
	return false
}

func envForSecret(key string) string { return envKeys[key] }

// decryptOverlay 把 config.json 里的 enc:v1: 值换回明文（只在内存里）。
func decryptOverlay(ov map[string]any) error {
	for _, key := range secretKeys {
		v, ok := getPath(ov, key)
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || !secret.IsEncrypted(s) {
			continue
		}
		plain, err := secret.Decrypt(s)
		if err != nil {
			return fmt.Errorf("%s 里的 %s：%w", "config.json", key, err)
		}
		setPath(ov, key, plain)
	}
	return nil
}

// overlayNeedsEncryption 判断 config.json 里是不是还有明文密钥。
func overlayNeedsEncryption(ov map[string]any) bool {
	for _, key := range secretKeys {
		if v, ok := getPath(ov, key); ok {
			if s, ok := v.(string); ok && s != "" && !secret.IsEncrypted(s) {
				return true
			}
		}
	}
	return false
}

// encryptedOverlay 返回要写盘的副本：密钥字段加密，其他原样。
func encryptedOverlay(ov map[string]any) (map[string]any, error) {
	out := cloneMap(ov)
	for _, key := range secretKeys {
		v, ok := getPath(out, key)
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		enc, err := secret.Encrypt(s)
		if err != nil {
			return nil, fmt.Errorf("加密 %s：%w", key, err)
		}
		setPath(out, key, enc)
	}
	return out, nil
}

// migrateFileSecrets 把 gateway.toml 里手写的明文密钥搬进 config.json（加密），
// 再把那一行改成空值。只改需要改的那几行，保留注释和格式；改完重新解析确认结果没变。
// 没有加载 secret.key（测试/嵌入式）时不动文件。
func (s *Store) migrateFileSecrets() []string {
	if !secret.Ready() {
		return nil
	}
	move := map[string]string{}
	for _, key := range secretKeys {
		if _, inOverlay := getPath(s.overlay, key); inOverlay {
			continue
		}
		if env := envForSecret(key); env != "" && os.Getenv(env) != "" {
			continue
		}
		parts := strings.Split(key, ".")
		if !s.md.IsDefined(parts...) {
			continue
		}
		v, _ := getPath(asMap(s.cur), key)
		text, ok := v.(string)
		if !ok || text == "" || secret.IsEncrypted(text) {
			continue
		}
		move[key] = text
	}
	if len(move) == 0 {
		return nil
	}
	content, err := rewriteTomlSecrets(s.path, move)
	if err != nil {
		return []string{fmt.Sprintf("没能把 gateway.toml 里的密钥搬到 config.json：%v", err)}
	}
	next := cloneMap(s.overlay)
	for k, v := range move {
		setPath(next, k, v)
	}
	ovJSON, _ := json.Marshal(next)
	cand, md, _, err := parseText(s.path, content, ovJSON)
	if err != nil {
		return []string{fmt.Sprintf("改完 gateway.toml 后配置读不了，没改：%v", err)}
	}
	if !sameConfig(s.cur, cand) {
		return []string{"把 gateway.toml 里的密钥改成空值后配置不一致，没改"}
	}
	// 先写加密后的 config.json，再改 gateway.toml；这样即使第二步失败也不会丢密钥。
	s.overlay = next
	if err := s.saveOverlay(next); err != nil {
		return []string{fmt.Sprintf("保存加密后的 config.json 失败，gateway.toml 没改：%v", err)}
	}
	if err := writeFileAtomic(s.path, content); err != nil {
		return []string{fmt.Sprintf("改 gateway.toml 失败：%v", err)}
	}
	s.cur, s.boot, s.md = cand, cand, md
	return []string{"已把 gateway.toml 里的明文密钥搬到 data/config.json（已加密），配置文件里那一行改成了空值"}
}

func sameConfig(a, b *Config) bool {
	am, bm := asMap(a), asMap(b)
	ab, _ := json.Marshal(am)
	bb, _ := json.Marshal(bm)
	return string(ab) == string(bb)
}

// rewriteTomlSecrets 只把 move 里的键所在行的值改成 ""，保留行尾注释；其余字节不动。
func rewriteTomlSecrets(path string, move map[string]string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	section := ""
	for _, line := range strings.SplitAfter(string(b), "\n") {
		raw := strings.TrimRight(line, "\n")
		nl := line[len(raw):]
		trim := strings.TrimSpace(raw)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			section = strings.Trim(trim, "[]")
		}
		if eq := strings.IndexByte(raw, '='); eq >= 0 {
			k := strings.TrimSpace(raw[:eq])
			if section != "" {
				k = section + "." + k
			}
			if _, ok := move[k]; ok && !strings.Contains(raw[eq+1:], "# 已加密") {
				rest := raw[eq+1:]
				comment := ""
				if i := tomlComment(rest); i >= 0 {
					comment = strings.TrimSpace(rest[i:])
				}
				line = raw[:eq+1] + ` ""`
				if comment != "" {
					line += "  " + comment
				}
				line += "  # 已加密保存在 data/config.json，在网页拓扑图对应的节点里改" + nl
			}
		}
		out.WriteString(line)
	}
	return out.String(), nil
}

// tomlComment 返回引号外的第一个 #；单双引号里的 # 是值的一部分，不是注释。
func tomlComment(s string) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == 0 {
			switch c {
			case '#':
				return i
			case '\'', '"':
				quote = c
			}
			continue
		}
		if quote == '"' && c == '\\' {
			i++
			continue
		}
		if c == quote {
			quote = 0
		}
	}
	return -1
}

func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		os.Chmod(tmp, st.Mode().Perm())
	}
	return os.Rename(tmp, path)
}

// parseText 用一大段 TOML 代替文件（给迁移时的“改完再读一遍”用）。
func parseText(path, content string, overlay []byte) (*Config, toml.MetaData, []string, error) {
	c := Config{Context: DefaultContext()}
	md, err := toml.Decode(content, &c)
	if err != nil {
		return nil, md, nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	return finish(&c, md, overlay, path, true)
}
