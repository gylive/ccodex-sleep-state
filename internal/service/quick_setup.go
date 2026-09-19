package service

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/codexconfig"
	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
)

// quickSetup is the explicitly confirmed "接入并修复" action. The caller holds
// the management lock and tells the user that unavailable state falls back to a
// single ordinary request. It never changes the model, login or system proxy.
func (c *control) quickSetup(ctx context.Context) error { return c.quickSetupMode(ctx, true) }

// Startup preserves an explicitly selected strict policy; a deliberate button
// click opts into passthrough as explained in its confirmation.
func (c *control) quickSetupMode(ctx context.Context, chooseFallback bool) error {
	if !c.configure {
		return errors.New("当前为只读模式，请用 setup 启动后再一键接入；没有修改 Codex 配置")
	}
	if c.rescue {
		return errors.New("请先备份并修复损坏的服务配置，再重新启动一键接入")
	}
	c.pause()
	if c.engine != nil && c.engine.Restricted() {
		c.resume()
		return errors.New("上游登录、权限或限流拦截尚未解除；一键接入不会清除等待状态或换出口重试")
	}
	previousEngine := c.engine
	healthyPrevious := c.setupError == "" && c.routeError == "" && (!c.managed || c.checkManaged() == nil)
	defer func() {
		// A failure before replacing the engine may resume a healthy old connection,
		// but must not revive a stale worker after CCS/authentication has changed.
		if c.engine == previousEngine && !healthyPrevious {
			c.stop()
			return
		}
		c.resume()
	}()
	next := c.config
	if chooseFallback || next.StateFallback == "" {
		next.StateFallback = "passthrough"
	}
	if !next.ChainEnabled() && next.Direct && len(next.ProxyURLs) == 0 && len(next.ProxyEnvs) == 0 && len(next.Subscriptions) == 0 && next.PinnedRoute == "" {
		proxy, err := discoverLocalSOCKS(ctx, []string{"127.0.0.1:7897", "127.0.0.1:7890", "127.0.0.1:10808"}, (&net.Dialer{}).DialContext)
		if err != nil {
			return err
		}
		if proxy != "" {
			next.Direct = false
			next.ProxyURLs = []string{proxy}
		}
	}
	if err := next.Validate(); err != nil {
		return err
	}
	// Building an adapter does not send a model request. Resolve failures before
	// touching Codex whenever possible; do not erase working user choices.
	routes, err := proxyroute.Load(ctx, next)
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			closeRoutes(routes)
		}
	}()
	if next.PinnedRoute != "" {
		found := false
		for _, route := range routes {
			found = found || route.ID == next.PinnedRoute
		}
		if !found {
			return errors.New("原来固定的出口已不存在，请在路由页切回自动；一键接入不会擅自更换你固定的线路")
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	c.stop()
	c.setup()
	if c.setupError != "" {
		preview, previewErr := codexconfig.PreviewRecovery(c.dir)
		if previewErr != nil {
			return errors.New("旧配置无法安全验证，请打开配置修复查看备份；一键接入没有强制覆盖")
		}
		if !preview.Needed {
			return errors.New(c.setupError)
		}
		if !preview.CanKeepCurrent {
			return errors.New("旧接管记录与当前配置冲突，不能自动保留合并。请打开配置修复选择备份恢复；一键接入不会丢弃你的新配置")
		}
		_, err = codexconfig.Recover(c.dir, codexconfig.RecoveryOptions{Mode: "keep_current", ExpectedConfigSHA256: preview.ConfigSHA256, ExpectedTransactionSHA256: preview.TransactionSHA256})
		if err != nil {
			return err
		}
		c.setup()
		if c.setupError != "" {
			return errors.New(c.setupError)
		}
	}
	if err = ctx.Err(); err != nil {
		c.setupError = "一键接入已取消，转发保持停止。配置备份仍在，请重新检查接入。"
		return err
	}
	if err = c.persist(next); err != nil {
		c.setupError = "服务设置未能保存，转发保持停止。Codex 备份仍在；请检查外部配置修改后重新接入。"
		return err
	}
	c.config = next
	c.start(routes)
	handedOff = true
	if c.engine == nil || c.routeError != "" {
		c.stop()
		return errors.New("配置已保存，但出口尚未准备好；请在路由页检查线路，当前没有转发请求")
	}
	return nil
}

type localDialer func(context.Context, string, string) (net.Conn, error)

// discoverLocalSOCKS probes only literal loopback addresses and only the SOCKS5
// no-auth negotiation. No destination, credential, subscription or prompt is
// sent. A successful greeting is not proof that its exit can reach the model.
func discoverLocalSOCKS(ctx context.Context, addresses []string, dial localDialer) (string, error) {
	for _, address := range addresses {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		conn, err := dial(probeCtx, "tcp", address)
		if err != nil {
			cancel()
			continue
		}
		deadline, _ := probeCtx.Deadline()
		_ = conn.SetDeadline(deadline)
		stop := context.AfterFunc(probeCtx, func() { _ = conn.Close() })
		count, writeErr := conn.Write([]byte{5, 1, 0})
		var reply [2]byte
		var readErr error
		if writeErr == nil && count == 3 {
			_, readErr = io.ReadFull(conn, reply[:])
		} else {
			readErr = io.ErrShortWrite
		}
		stop()
		_ = conn.Close()
		cancel()
		if writeErr == nil && readErr == nil && reply == [2]byte{5, 0} {
			return "socks5://" + address, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", nil
}
