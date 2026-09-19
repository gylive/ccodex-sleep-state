package proxyroute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/metacubex/mihomo/adapter/outbound"
	C "github.com/metacubex/mihomo/constant"
)

// chainDialer gives the second proxy a connection through the first proxy.
// It never uses a global Mihomo registry, Clash's API, or system proxy settings.
type chainDialer struct{ transport *http.Transport }

func (d chainDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.transport.DialContext(ctx, network, address)
}
func (d chainDialer) ListenPacket(context.Context, string, string, netip.AddrPort) (net.PacketConn, error) {
	return nil, errors.New("采集链式代理只支持 TCP")
}

func BuildProbeChain(c settings.ProbeChain) (Route, error) {
	if err := c.Validate(); err != nil {
		return Route{}, err
	}
	exits, _ := c.ExitURLs()
	if len(exits) != 1 {
		return Route{}, errors.New("构建单个链式出口时须提供一个落地地址")
	}
	return buildProbeChain(c.LocalProxy, exits[0])
}

func BuildProbeChains(c settings.ProbeChain) ([]Route, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	exits, _ := c.ExitURLs()
	routes := make([]Route, 0, len(exits))
	for i, exit := range exits {
		route, err := buildProbeChain(c.LocalProxy, exit)
		if err != nil {
			for _, built := range routes {
				built.Close()
			}
			return nil, fmt.Errorf("第 %d 个链式采集出口构建失败", i+1)
		}
		route.DisplayName = fmt.Sprintf("落地出口 %d（仅采集）", i+1)
		routes = append(routes, route)
	}
	return routes, nil
}

// Every route has a first-hop-only transport for normal requests and a separate
// two-hop transport for probes. Moving the probe cursor cannot move user traffic
// onto a landing proxy, including when there is no usable state.
func buildProbeChain(localURL, exitURL string) (Route, error) {
	node, err := ParseURI(localURL)
	if err != nil {
		return Route{}, errors.New("本地代理地址无效")
	}
	local, err := Build(node, 0)
	if err != nil {
		return Route{}, err
	}
	u, _ := url.Parse(exitURL) // validated above
	port, _ := strconv.Atoi(u.Port())
	user, password := "", ""
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	basic := outbound.BasicOption{DialerForAPI: chainDialer{local.Transport}}
	var exit C.ProxyAdapter
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		exit, err = outbound.NewSocks5(outbound.Socks5Option{BasicOption: basic, Name: "probe-exit", Server: u.Hostname(), Port: port, UserName: user, Password: password})
	} else {
		exit, err = outbound.NewHttp(outbound.HttpOption{BasicOption: basic, Name: "probe-exit", Server: u.Hostname(), Port: port, UserName: user, Password: password, TLS: u.Scheme == "https"})
	}
	if err != nil {
		local.Close()
		return Route{}, errors.New("无法构建采集落地代理")
	}
	probe := baseTransport()
	probe.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		metadata := &C.Metadata{NetWork: C.TCP, Type: C.INNER}
		if err := metadata.SetRemoteAddress(address); err != nil {
			return nil, errors.New("采集目标地址无效")
		}
		conn, err := exit.DialContext(ctx, metadata)
		if err != nil {
			return nil, errors.New("采集链式代理连接失败，请检查本地代理及落地出口")
		}
		return conn, nil
	}
	id, _ := nodeIdentity(map[string]any{"local": localURL, "exit": exitURL, "scope": "probe-only"})
	return Route{ID: id, StableID: id, DisplayName: "本地代理（采集经链式落地）", Protocol: "probe-chain", Transport: local.Transport, ProbeTransport: probe, close: func() error {
		local.Close()
		return exit.Close()
	}}, nil
}
