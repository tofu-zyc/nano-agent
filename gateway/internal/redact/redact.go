// Package redact 把工具结果里的密钥打星。只处理工具结果，用户自己发的消息不动。
//
// 先精确替换网关知道的密钥原文，再按常见特征兜底。宁可漏掉，也不要误伤普通代码：
// 特征都有比较长的最小长度，匹配到的值保留前 4 位和后 4 位。
package redact

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[bp]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key)(\s*[:=]\s*["'])([^"'\s]{16,})(["'])`),
}

// Mask 保留前 4 位和后 4 位；太短就整个打星。
func Mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

// Needed 判断这个模式在这个场景下要不要脱敏。public 表示模型接口在公网；publicChannel
// 表示 Telegram/飞书这种公网聊天渠道。
func Needed(mode string, publicModel, publicChannel bool) bool {
	switch mode {
	case "always":
		return true
	case "public":
		return publicModel || publicChannel
	default:
		return false
	}
}

// ToolResult 脱敏一段工具结果。known 是网关自己保存的密钥原文，先精确替换；
// then 再按常见特征兜底。返回结果对同一段输入总是相同。
func ToolResult(text string, known []string) string {
	return toolText(jobChunks(text, known), known)
}

func toolText(text string, known []string) string {
	if text == "" {
		return text
	}
	known = append([]string(nil), known...)
	sort.SliceStable(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, k := range known {
		if len(k) < 8 {
			continue
		}
		text = strings.ReplaceAll(text, k, Mask(k))
	}
	for _, re := range patterns {
		text = re.ReplaceAllStringFunc(text, func(s string) string {
			if strings.HasPrefix(s, "-----BEGIN") {
				lines := strings.Split(s, "\n")
				if len(lines) >= 2 {
					return lines[0] + "\n" + strings.Repeat("*", 24) + "\n" + lines[len(lines)-1]
				}
				return strings.Repeat("*", len(s))
			}
			// password/token = "..."：保留键、引号，只给值打星
			if m := regexp.MustCompile(`(?i)^(.*?[:=]\s*["'])(.*?)(["'])$`).FindStringSubmatch(s); m != nil {
				return m[1] + Mask(m[2]) + m[3]
			}
			return Mask(s)
		})
	}
	return text
}

// IsPublicBaseURL 判断模型接口是不是在本机或内网：域名一律算公网，只有解析出的 IP
// 是 loopback、10/8、172.16/12、192.168/16、100.64/10、fc00::/7 才算内网。
func IsPublicBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"} {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
