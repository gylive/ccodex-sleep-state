package settings

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ProbeChain is private configuration. Status APIs must never serialize it.
// Only probes use the exits; normal traffic uses LocalProxy alone.
type ProbeChain struct {
	Enabled     bool     `json:"enabled"`
	LocalProxy  string   `json:"local_proxy"`
	ExitProxy   string   `json:"exit_proxy,omitempty"` // legacy single-exit configuration
	ExitProxies []string `json:"exit_proxies,omitempty"`
}

const MaxProbeExits = 256
const MaxProbeExitTextBytes = 256 << 10

func (c Config) ChainEnabled() bool { return c.ProbeChain != nil && c.ProbeChain.Enabled }

// ExitURLs returns an independent, normalized, deduplicated list in input order.
// Reject ambiguous configs instead of silently dropping an old or new exit.
func (c ProbeChain) ExitURLs() ([]string, error) {
	if c.ExitProxy != "" && len(c.ExitProxies) != 0 {
		return nil, errors.New("落地出口不能同时配置 exit_proxy 和 exit_proxies")
	}
	exits := c.ExitProxies
	if c.ExitProxy != "" {
		exits = []string{c.ExitProxy}
	}
	return UniqueProbeExits(exits)
}

func UniqueProbeExits(exits []string) ([]string, error) {
	result := make([]string, 0, min(len(exits), MaxProbeExits))
	seen := make(map[string]bool)
	for i, raw := range exits {
		u, err := proxyEndpoint(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个落地出口地址无效，请检查协议、主机和端口", i+1)
		}
		value := canonicalProxyEndpoint(u)
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
			if len(result) > MaxProbeExits {
				return nil, fmt.Errorf("最多支持 %d 个不同的落地出口", MaxProbeExits)
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("请至少填写一个落地出口")
	}
	return result, nil
}

// ParseProbeExits accepts a multiline panel input without returning credentials
// in errors. The whole batch is rejected if any non-comment line is invalid.
func ParseProbeExits(text, protocol string) ([]string, error) {
	if len(text) > MaxProbeExitTextBytes {
		return nil, errors.New("落地出口列表超过 256 KiB，请减少内容")
	}
	var exits []string
	for i, line := range strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		value, err := NormalizeProxyEndpoint(line, protocol)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行落地出口格式不正确，请填写 user:passwd@host:port 或完整代理 URL", i+1)
		}
		exits = append(exits, value)
	}
	return UniqueProbeExits(exits)
}

func canonicalProxyEndpoint(u *url.URL) string {
	copyURL := *u
	port, _ := strconv.Atoi(u.Port())
	copyURL.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(port))
	copyURL.Path, copyURL.RawPath = "", ""
	if copyURL.Scheme == "socks5h" {
		copyURL.Scheme = "socks5"
	}
	return copyURL.String()
}

func (c ProbeChain) Validate() error {
	local, err := proxyEndpoint(c.LocalProxy)
	if err != nil || !IsLoopback(local.Hostname()) {
		return errors.New("本地代理须为 http/https/socks5://127.0.0.1:端口 或 [::1]:端口")
	}
	exits, err := c.ExitURLs()
	if err != nil {
		return err
	}
	for i, raw := range exits {
		exit, _ := url.Parse(raw)
		localPort, _ := strconv.Atoi(local.Port())
		if strings.EqualFold(local.Hostname(), exit.Hostname()) && strconv.Itoa(localPort) == exit.Port() {
			return fmt.Errorf("第 %d 个落地出口不能与本地代理为同一个端点", i+1)
		}
	}
	return nil
}

func proxyEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t\\ ") {
		return nil, errors.New("invalid proxy endpoint")
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, errors.New("unsupported proxy protocol")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("invalid proxy endpoint")
	}
	return u, nil
}

// NormalizeProxyEndpoint accepts the panel's user:password@host:port shorthand.
// Full URLs use standard percent encoding; shorthand credentials are literal.
func NormalizeProxyEndpoint(raw, protocol string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if protocol == "" {
			protocol = "http"
		}
		u := &url.URL{Scheme: protocol, Host: raw}
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			user, password, ok := strings.Cut(raw[:at], ":")
			if !ok || user == "" || strings.ContainsAny(raw[:at], "\r\n\t\\") {
				return "", errors.New("请填写 user:passwd@host:port；@ 前不需要反斜杠")
			}
			u.User, u.Host = url.UserPassword(user, password), raw[at+1:]
		}
		raw = u.String()
	}
	u, err := proxyEndpoint(raw)
	if err != nil {
		return "", errors.New("代理地址无效，请检查协议、主机和端口；完整 URL 的密码特殊字符须做 URL 编码")
	}
	return canonicalProxyEndpoint(u), nil
}
