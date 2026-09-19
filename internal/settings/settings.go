// Package settings owns paths and validation, not network or process state.
package settings

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const Model = "gpt-6-astra"
const App = "ccodex-sleep-state"

type Source struct {
	File             string   `json:"file,omitempty"`
	URL              string   `json:"url,omitempty"`
	URLEnv           string   `json:"url_env,omitempty"`
	UserAgent        string   `json:"user_agent,omitempty"`
	IncludeProtocols []string `json:"include_protocols,omitempty"`
	ExcludeKeywords  []string `json:"exclude_keywords,omitempty"`
}

type Config struct {
	ProbeChain        *ProbeChain `json:"probe_chain,omitempty"`
	ExternalProxyOnly bool        `json:"external_proxy_only,omitempty"`
	StateRefreshMode  string      `json:"state_refresh_mode,omitempty"`
	RequestLimitMiB   int         `json:"request_limit_mib,omitempty"`
	ZstdWindowMiB     int         `json:"zstd_window_mib,omitempty"`
	CompactLimitMiB   int         `json:"compact_limit_mib,omitempty"`
	EgressMode        string      `json:"egress_mode,omitempty"`
	EgressRoute       string      `json:"egress_route,omitempty"`
	PoolEnabled       bool        `json:"pool_enabled,omitempty"`

	StateFallback        string   `json:"state_fallback,omitempty"`
	Model                string   `json:"model,omitempty"`
	AccountMode          string   `json:"account_mode,omitempty"`
	UpstreamMode         string   `json:"upstream_mode,omitempty"`
	UpstreamKind         string   `json:"upstream_kind,omitempty"`
	CodexProfile         string   `json:"codex_profile,omitempty"`
	InjectionDisabled    bool     `json:"injection_disabled,omitempty"`
	PinnedRoute          string   `json:"pinned_route,omitempty"`
	Listen               string   `json:"listen"`
	Upstream             string   `json:"upstream"`
	CodexHome            string   `json:"codex_home,omitempty"`
	Direct               bool     `json:"direct"`
	ProxyURLs            []string `json:"proxy_urls"`
	ProxyEnvs            []string `json:"proxy_envs"`
	Subscriptions        []Source `json:"subscriptions"`
	SubscriptionProxyEnv string   `json:"subscription_proxy_env,omitempty"`
	ProbeSeconds         int      `json:"probe_timeout_seconds"`
	RefreshSeconds       int      `json:"refresh_before_seconds"`
	CooldownSeconds      int      `json:"probe_cooldown_seconds"`
	MaxProbes            int      `json:"max_probes_per_round"`
	TTLSeconds           int      `json:"state_ttl_seconds"`
	BaselineBlocks       int      `json:"baseline_blocks"`
}

func Default() Config {
	return Config{StateRefreshMode: "on_demand", RequestLimitMiB: 64, ZstdWindowMiB: 64, CompactLimitMiB: 64, EgressMode: "state", Model: Model, AccountMode: "auto", UpstreamKind: "official", Listen: "127.0.0.1:17841", Upstream: "https://chatgpt.com/backend-api/codex", Direct: true,
		ProxyURLs: []string{}, ProxyEnvs: []string{}, Subscriptions: []Source{}, ProbeSeconds: 20,
		RefreshSeconds: 1200, CooldownSeconds: 180, MaxProbes: 6, TTLSeconds: 3600, BaselineBlocks: 10}
}

func Load(path string) (Config, error) {
	c := Default()
	// Existing files without this field retain the previous standby behavior.
	c.StateRefreshMode = "standby"
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open service config; run init first")
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil {
		return c, errors.New("invalid service config JSON (unknown fields are rejected)")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return c, errors.New("service config contains trailing data")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.ChainEnabled() {
		if err := c.ProbeChain.Validate(); err != nil {
			return err
		}
		if c.PoolEnabled || c.PinnedRoute != "" || c.EgressRoute != "" || (c.EgressMode != "" && c.EgressMode != "state") {
			return errors.New("采集链式模式使用固定本地代理，请关闭随机/固定节点及用后移出策略")
		}
	}
	if c.StateRefreshMode != "" && c.StateRefreshMode != "standby" && c.StateRefreshMode != "on_demand" {
		return errors.New("state 策略必须为 standby 或 on_demand")
	}

	for _, n := range []int{c.RequestLimitMiB, c.ZstdWindowMiB, c.CompactLimitMiB} {
		if n != 0 && (n < 16 || n > 128) {
			return errors.New("请求/解压窗口/压缩回复上限必须是 16–128 MiB")
		}
	}
	if c.EgressMode != "" && c.EgressMode != "state" && c.EgressMode != "random" && c.EgressMode != "fixed" {
		return errors.New("出口模式必须为 state、random 或 fixed")
	}
	if c.EgressMode == "fixed" && c.EgressRoute == "" {
		return errors.New("固定出口需要选择一个节点")
	}

	if c.StateFallback != "" && c.StateFallback != "strict" && c.StateFallback != "passthrough" {
		return errors.New("state_fallback must be strict or passthrough")
	}
	if !SupportedModel(c.SelectedModel()) {
		return errors.New("model must be gpt-6-astra, gpt-5.6-sol or gpt-5.6-terra")
	}
	if c.AccountMode != "" && c.AccountMode != "auto" && c.AccountMode != "personal" && c.AccountMode != "team" {
		return errors.New("account_mode must be auto, personal or team")
	}
	if c.UpstreamMode != "" && c.UpstreamMode != "auto" && c.UpstreamMode != "manual" {
		return errors.New("upstream_mode must be auto or manual")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	ip := net.ParseIP(host)
	p, pe := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || pe != nil || p < 1 || p > 65535 {
		return errors.New("listen must be a literal loopback IP and port (1–65535)")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid upstream URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && IsLoopback(u.Hostname())) {
		return errors.New("upstream must use HTTPS; HTTP is allowed only for a loopback test server")
	}
	if c.UpstreamKind != "" && c.UpstreamKind != "official" && c.UpstreamKind != "relay" {
		return errors.New("upstream_kind must be official or relay")
	}
	if u.RawPath != "" || strings.Contains(u.Path, "..") || strings.ContainsAny(u.Path, "\\\r\n") {
		return errors.New("upstream base path must not contain escapes or traversal")
	}
	if !c.IsRelay() && strings.TrimRight(u.Path, "/") != "/backend-api/codex" {
		return errors.New("upstream path must be /backend-api/codex")
	}
	if c.ProbeSeconds < 1 || c.ProbeSeconds > 60 || c.MaxProbes < 1 || c.MaxProbes > 20 || c.CooldownSeconds < 30 || c.CooldownSeconds > 3600 {
		return errors.New("probe limits: timeout 1–60s, round 1–20 requests, cooldown 30–3600s")
	}
	if c.TTLSeconds < 120 || c.TTLSeconds > 3600 || c.RefreshSeconds < 30 || c.RefreshSeconds >= c.TTLSeconds || c.BaselineBlocks < 1 || c.BaselineBlocks > 32 {
		return errors.New("invalid state policy; refresh must be shorter than TTL (120–3600s)")
	}
	for _, source := range c.Subscriptions {
		if len(source.IncludeProtocols) > 16 || len(source.ExcludeKeywords) > 64 {
			return errors.New("too many subscription filter entries")
		}
		for _, keyword := range source.ExcludeKeywords {
			if strings.TrimSpace(keyword) == "" || len(keyword) > 128 {
				return errors.New("subscription exclude keyword must contain 1–128 bytes")
			}
		}
		if len(source.UserAgent) > 256 || strings.ContainsAny(source.UserAgent, "\r\n") {
			return errors.New("subscription user_agent must be one line, at most 256 bytes")
		}
	}
	if len(c.ProxyURLs)+len(c.ProxyEnvs) > 256 || len(c.Subscriptions) > 16 {
		return errors.New("too many proxy sources")
	}
	return nil
}

func IsLoopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }

func DataDir() (string, error) {
	if p := os.Getenv("CCODEX_STATE_HOME"); p != "" {
		return filepath.Abs(p)
	}
	if runtime.GOOS == "windows" {
		if p := os.Getenv("LOCALAPPDATA"); p != "" {
			return filepath.Join(p, App), nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, App), nil
}

func (c Config) CodexDir() (string, error) {
	if c.CodexHome != "" {
		return filepath.Abs(c.CodexHome)
	}
	if p := os.Getenv("CODEX_HOME"); p != "" {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// IsRelay distinguishes user-selected API-key providers from the official login flow.
func (c Config) IsRelay() bool { return c.UpstreamKind == "relay" }

// SelectedModel preserves configs written before the model selector existed.
func (c Config) SelectedModel() string {
	if c.Model == "" {
		return Model
	}
	return c.Model
}
func SupportedModel(model string) bool {
	switch model {
	case Model, "gpt-5.6-sol", "gpt-5.6-terra":
		return true
	}
	return false
}
func SupportedModels() []string { return []string{Model, "gpt-5.6-sol", "gpt-5.6-terra"} }

func (c Config) RequestBytes() int64 {
	if c.RequestLimitMiB == 0 {
		return 64 << 20
	}
	return int64(c.RequestLimitMiB) << 20
}
func (c Config) WindowBytes() uint64 {
	if c.ZstdWindowMiB == 0 {
		return 64 << 20
	}
	return uint64(c.ZstdWindowMiB) << 20
}
func (c Config) CompactBytes() int64 {
	if c.CompactLimitMiB == 0 {
		return 64 << 20
	}
	return int64(c.CompactLimitMiB) << 20
}
