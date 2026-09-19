package proxyroute

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
	corelog "github.com/metacubex/mihomo/log"
)

// A route is immutable once loaded. States refer to its index. A probe-only
// chain has two transports; ordinary routes share one transport for both uses.
type Route struct {
	ID string
	// StableID identifies connection settings, independent of list order or display name.
	StableID string
	// DisplayName and Protocol are for the authenticated local panel, never logs.
	DisplayName string
	Protocol    string
	Transport   *http.Transport
	// ProbeTransport is set only for the explicit probe-only chain mode.
	ProbeTransport *http.Transport
	close          func() error
}

var quietOnce sync.Once

func QuietCore() {
	quietOnce.Do(func() { corelog.SetLevel(corelog.SILENT) })
}

func (r Route) Close() {
	r.Transport.CloseIdleConnections()
	if r.ProbeTransport != nil {
		r.ProbeTransport.CloseIdleConnections()
	}
	if r.close != nil {
		_ = r.close()
	}
}

func baseTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second,
		MaxIdleConns: 32, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 16, MaxResponseHeaderBytes: 1 << 20}
}

func Build(node map[string]any, index int) (Route, error) {
	id := fmt.Sprintf("route-%03d", index+1)
	if err := validateNode(node); err != nil {
		return Route{}, err
	}
	copyNode := make(map[string]any, len(node))
	for k, v := range node {
		copyNode[k] = v
	}
	stableID, err := nodeIdentity(copyNode)
	if err != nil {
		return Route{}, err
	}
	copyNode["name"] = id
	proxy, err := adapter.ParseProxy(copyNode)
	if err != nil {
		return Route{}, errors.New("node rejected by outbound core; check protocol fields")
	}
	tr := baseTransport()
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		metadata := &C.Metadata{NetWork: C.TCP, Type: C.INNER}
		if err := metadata.SetRemoteAddress(address); err != nil {
			return nil, errors.New("invalid upstream address")
		}
		conn, err := proxy.DialContext(ctx, metadata)
		if err != nil {
			return nil, errors.New("proxy connection failed")
		}
		return conn, nil
	}
	protocol, _ := node["type"].(string)
	return Route{ID: id, StableID: stableID, DisplayName: nodeDisplayName(node, protocol), Protocol: protocol, Transport: tr, close: proxy.Close}, nil
}

// Labels come only from a subscription's explicit display name, never from
// its address or credentials. URI-like names are replaced rather than redacted.
func nodeDisplayName(node map[string]any, protocol string) string {
	name, _ := node["name"].(string)
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name))
	fallback := protocol + " 线路"
	if name == "" || name == "imported" || strings.Contains(name, "://") || strings.Contains(name, "@") {
		return fallback
	}
	for _, key := range []string{"server", "password", "username", "uuid"} {
		value, _ := node[key].(string)
		if value != "" && strings.Contains(name, value) {
			return fallback
		}
	}
	runes := []rune(name)
	if len(runes) > 80 {
		name = string(runes[:80])
	}
	return name
}

// JSON encoding sorts map keys; display labels never change a pinned endpoint.
// Only the digest is exposed: raw addresses and credentials stay private.
func nodeIdentity(node map[string]any) (string, error) {
	canonical := make(map[string]any, len(node))
	for key, value := range node {
		if key != "name" {
			canonical[key] = value
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", errors.New("node settings cannot be canonicalized")
	}
	digest := sha256.Sum256(data)
	return "route-" + hex.EncodeToString(digest[:16]), nil
}

func Load(ctx context.Context, c settings.Config) ([]Route, error) {
	QuietCore()
	if c.ChainEnabled() {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return BuildProbeChains(*c.ProbeChain)
	}
	var routes []Route
	success := false
	defer func() {
		if !success {
			for _, route := range routes {
				route.Close()
			}
		}
	}()
	if c.Direct {
		routes = append(routes, Route{ID: "direct", StableID: "direct", DisplayName: "直连", Protocol: "direct", Transport: baseTransport()})
	}
	seen := make(map[string]bool)
	add := func(nodes []map[string]any) error {
		for _, node := range nodes {
			if c.ExternalProxyOnly {
				kind, _ := node["type"].(string)
				if kind != "http" && kind != "socks5" {
					return errors.New("当前是外部代理模式，只接受 HTTP/SOCKS5 端点；请在自己的代理客户端加载此订阅，再填写本地端口")
				}
			}

			route, err := Build(node, len(routes))
			if err != nil {
				return fmt.Errorf("node %d: %w", len(routes)+1, err)
			}
			route.ID = route.StableID
			if seen[route.ID] {
				route.Close()
				continue
			}
			if len(routes) >= MaxNodes {
				route.Close()
				return errors.New("合并后的代理池超过 256 个不同节点，请减少订阅或设置筛选条件")
			}
			seen[route.ID] = true
			routes = append(routes, route)
		}
		return nil
	}
	urls := append([]string(nil), c.ProxyURLs...)
	for _, name := range c.ProxyEnvs {
		v := os.Getenv(name)
		if v == "" {
			return nil, errors.New("a configured proxy environment variable is empty")
		}
		urls = append(urls, v)
	}
	for _, raw := range urls {
		node, err := ParseURI(raw)
		if err != nil {
			return nil, errors.New("invalid proxy URI in service config")
		}
		if err = add([]map[string]any{node}); err != nil {
			return nil, err
		}
	}
	download := baseTransport()
	defer download.CloseIdleConnections()
	if c.SubscriptionProxyEnv != "" {
		raw := os.Getenv(c.SubscriptionProxyEnv)
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("invalid subscription download proxy environment variable")
		}
		download.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Transport: download, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 || r.URL.Scheme != "https" {
			return errors.New("subscription redirect rejected")
		}
		return nil
	}}
	for i, source := range c.Subscriptions {
		raw := source.URL
		if source.URLEnv != "" {
			if raw != "" {
				return nil, errors.New("use url or url_env, not both")
			}
			raw = os.Getenv(source.URLEnv)
		}
		var data []byte
		var err error
		if source.File != "" {
			if raw != "" || source.URLEnv != "" {
				return nil, errors.New("local file cannot be combined with a subscription URL")
			}
			data, err = readLocal(source.File)
		} else {
			data, err = fetch(ctx, client, raw, source.UserAgent)
		}
		if err != nil {
			return nil, fmt.Errorf("subscription %d: %w", i+1, err)
		}
		nodes, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("subscription %d: %w", i+1, err)
		}
		nodes = filterNodes(nodes, source)
		if len(nodes) == 0 {
			return nil, fmt.Errorf("subscription %d: no nodes remain after filters", i+1)
		}
		if err = add(nodes); err != nil {
			return nil, err
		}
	}
	if len(routes) == 0 {
		return nil, errors.New("no egress configured; add a subscription/proxy or enable direct")
	}
	success = true
	return routes, nil
}

func fetch(ctx context.Context, client *http.Client, raw, userAgent string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && settings.IsLoopback(u.Hostname()))) {
		return nil, errors.New("subscription must be an HTTPS URL (loopback HTTP is allowed for tests)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid subscription URL")
	}
	if userAgent == "" {
		userAgent = "ccodex-sleep-state/1"
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("download failed; check connectivity and subscription validity")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubscriptionBytes+1))
	if err != nil {
		return nil, errors.New("cannot read subscription")
	}
	if len(data) > MaxSubscriptionBytes {
		return nil, errors.New("subscription exceeds 2 MiB")
	}
	return data, nil
}

// Filtering precedes adapter construction: excluded nodes never dial anything.
// Keywords refer to subscription labels/addresses, not verified exit geography.
func filterNodes(nodes []map[string]any, source settings.Source) []map[string]any {
	selected := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		protocol, _ := node["type"].(string)
		if len(source.IncludeProtocols) > 0 {
			matched := false
			for _, allowed := range source.IncludeProtocols {
				matched = matched || strings.EqualFold(protocol, allowed)
			}
			if !matched {
				continue
			}
		}
		name, _ := node["name"].(string)
		host, _ := node["server"].(string)
		label := strings.ToLower(name + " " + host)
		excluded := false
		for _, word := range source.ExcludeKeywords {
			if strings.Contains(label, strings.ToLower(word)) {
				excluded = true
				break
			}
		}
		if !excluded {
			selected = append(selected, node)
		}
	}
	return selected
}

// readLocal reads only an explicitly selected regular subscription file. Never
// open a named pipe, device, or symlink supplied as a subscription.
func readLocal(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("local subscription must be an existing regular file")
	}
	if info.Size() > MaxSubscriptionBytes {
		return nil, errors.New("subscription exceeds 2 MiB")
	}
	// Nonblocking open prevents a file swapped for a FIFO between Lstat and
	// Open from hanging. Windows treats this flag as a no-op for regular files.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open local subscription")
	}
	defer f.Close()
	opened, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		return nil, errors.New("local subscription changed while opening; select the file again")
	}
	if opened.Size() > MaxSubscriptionBytes {
		return nil, errors.New("subscription exceeds 2 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSubscriptionBytes+1))
	if err != nil || len(data) > MaxSubscriptionBytes {
		return nil, errors.New("cannot read local subscription within 2 MiB limit")
	}
	return data, nil
}
