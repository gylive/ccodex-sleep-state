package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"github.com/gylive/ccodex-sleep-state/internal/settings"
)

type probeChainRequest struct {
	Enabled     bool   `json:"enabled"`
	LocalProxy  string `json:"local_proxy"`
	ExitProxy   string `json:"exit_proxy"`   // legacy single-line field
	ExitProxies string `json:"exit_proxies"` // newline-separated batch field
	Append      bool   `json:"append"`
	Protocol    string `json:"protocol"`
}

func (c *control) chainCandidate(v probeChainRequest) (settings.Config, error) {
	next := c.config
	chain := settings.ProbeChain{}
	if next.ProbeChain != nil {
		chain = *next.ProbeChain
	}
	chain.Enabled = v.Enabled
	var err error
	if strings.TrimSpace(v.LocalProxy) != "" {
		chain.LocalProxy, err = settings.NormalizeProxyEndpoint(v.LocalProxy, "http")
		if err != nil {
			return next, err
		}
	}
	if strings.TrimSpace(v.ExitProxies) != "" {
		parsed, parseErr := settings.ParseProbeExits(v.ExitProxies, v.Protocol)
		err = parseErr
		if err != nil {
			return next, err
		}
		if v.Append {
			var old []string
			if chain.ExitProxy != "" || len(chain.ExitProxies) > 0 {
				old, err = chain.ExitURLs()
				if err != nil {
					return next, err
				}
			}
			parsed = append(old, parsed...)
			parsed, err = settings.UniqueProbeExits(parsed)
			if err != nil {
				return next, err
			}
		}
		chain.ExitProxy = ""
		chain.ExitProxies = parsed
	} else if strings.TrimSpace(v.ExitProxy) != "" {
		chain.ExitProxies = nil
		chain.ExitProxy, err = settings.NormalizeProxyEndpoint(v.ExitProxy, v.Protocol)
		if err != nil {
			return next, err
		}
	}
	next.ProbeChain = &chain
	if chain.Enabled {
		// The two transports share one logical route. A consumable or random pool
		// would make ordinary traffic rotate or stop after the first probe.
		next.PoolEnabled = false
		next.PinnedRoute, next.EgressRoute, next.EgressMode = "", "", "state"
	}
	return next, next.Validate()
}

func (c *control) chainStatus() map[string]any {
	configured := false
	count := 0
	if c.config.ProbeChain != nil && c.config.ProbeChain.LocalProxy != "" {
		if exits, err := c.config.ProbeChain.ExitURLs(); err == nil {
			configured, count = len(exits) > 0, len(exits)
		}
	}
	protocol := "http"
	if configured {
		if exits, err := c.config.ProbeChain.ExitURLs(); err == nil {
			u, err := url.Parse(exits[0])
			if err != nil {
				return map[string]any{"enabled": c.config.ChainEnabled(), "configured": configured, "count": count, "protocol": protocol}
			}
			protocol = u.Scheme
			if protocol == "socks5h" {
				protocol = "socks5"
			}
		}
	}
	return map[string]any{"enabled": c.config.ChainEnabled(), "configured": configured, "count": count, "protocol": protocol}
}

func (c *control) chainAPI(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	fail := func(err error) { reply(w, 400, map[string]string{"error": err.Error()}) }
	if c.rescue {
		fail(errors.New("请先修复服务配置"))
		return
	}
	var v probeChainRequest
	if err := decode(w, r, &v); err != nil {
		fail(err)
		return
	}
	test := r.URL.Path == "/admin/api/probe-chain/test"
	if test {
		v.Enabled = true
	}
	next, err := c.chainCandidate(v)
	if err != nil {
		fail(err)
		return
	}
	if test {
		routes, err := proxyroute.BuildProbeChains(*next.ProbeChain)
		if err != nil {
			fail(err)
			return
		}
		defer func() {
			for _, route := range routes {
				route.Close()
			}
		}()
		u, err := url.Parse(c.effective().Upstream)
		if err != nil || u.Hostname() == "" {
			fail(errors.New("当前上游地址无效，请先修复连接设置"))
			return
		}
		port := u.Port()
		if port == "" {
			port = "443"
			if u.Scheme == "http" {
				port = "80"
			}
		}
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		failed := 0
		for _, route := range routes {
			conn, dialErr := route.ProbeTransport.DialContext(testCtx, "tcp", net.JoinHostPort(u.Hostname(), port))
			if dialErr != nil {
				failed++
				continue
			}
			conn.Close()
		}
		if failed == len(routes) {
			fail(errors.New("所有链式出口连接均未通过，请检查本地代理端口、落地协议、账号和密码"))
			return
		}
		reply(w, 200, map[string]any{"message": fmt.Sprintf("%d 个链式出口中 %d 个已建立到上游的 TCP 隧道。没有发送模型请求、没有保存设置；这不证明能采到 292 或跨出口注入有效。", len(routes), len(routes)-failed), "total": len(routes), "failed": failed})
		return
	}
	if err := c.applyConfig(ctx, next); err != nil {
		fail(err)
		return
	}
	message := "已关闭采集链式模式，按当前出口策略恢复原有代理来源。需要随机出口或用后移出时，请在高级设置重新选择。旧 state 已清空。"
	if next.ChainEnabled() {
		message = "已保存：仅 state 采集经本地代理 → 落地出口；正常回复、压缩及模型列表只经本地代理。原代理来源保留但暂停使用，随机出口和用后移出已关闭。旧 state 已清空。"
	}
	reply(w, 200, map[string]string{"message": message})
}
