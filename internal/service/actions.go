package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gylive/ccodex-sleep-state/internal/codexconfig"
	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"io"
	"net/http"
	"strings"
	"time"
)

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("请求格式错误，或内容超过 1 MiB")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("请求包含多余内容")
	}
	return nil
}

type sourceRequest struct {
	EnablePool bool     `json:"enable_pool"`
	Append     bool     `json:"append"`
	Mode       string   `json:"mode"`
	Value      string   `json:"value"`
	UserAgent  string   `json:"user_agent"`
	Exclude    []string `json:"exclude_keywords"`
	Protocols  []string `json:"include_protocols"`
}

func (c *control) candidate(v sourceRequest) (settings.Config, error) {
	next := c.config
	if next.ChainEnabled() {
		return next, errors.New("采集链式模式正在使用独立的本地代理和落地出口；请先关闭该模式，再修改原代理池来源")
	}
	if !v.Append {
		next.Direct = false
		next.ProxyURLs = []string{}
		next.ProxyEnvs = []string{}
		next.Subscriptions = []settings.Source{}
		next.PinnedRoute = ""
		next.EgressRoute = ""
		if next.EgressMode == "fixed" {
			next.EgressMode = "random"
		}
	}
	next.Subscriptions = append([]settings.Source(nil), next.Subscriptions...)
	next.ProxyURLs = append([]string(nil), next.ProxyURLs...)
	if v.EnablePool {
		next.PoolEnabled = true
		next.PinnedRoute = ""
		if next.EgressMode != "fixed" {
			next.EgressMode = "random"
		}
	}
	source := settings.Source{UserAgent: v.UserAgent, ExcludeKeywords: v.Exclude, IncludeProtocols: v.Protocols}
	switch v.Mode {
	case "direct":
		next.Direct = true
	case "proxy":
		next.ProxyURLs = append(next.ProxyURLs, v.Value)
	case "subscription":
		source.URL = v.Value
		next.Subscriptions = append(next.Subscriptions, source)
	case "subscription-list":
		seen := map[string]bool{}
		for _, existing := range next.Subscriptions {
			seen[existing.URL] = true
		}
		count := 0
		for _, line := range strings.Split(strings.TrimPrefix(v.Value, "\ufeff"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			count++
			if !seen[line] {
				entry := source
				entry.URL = line
				next.Subscriptions = append(next.Subscriptions, entry)
				seen[line] = true
			}
		}
		if count == 0 {
			return next, errors.New("订阅列表为空，请每行填写一个 HTTPS 订阅链接")
		}
	case "file":
		source.File = v.Value
		next.Subscriptions = append(next.Subscriptions, source)
	default:
		return next, errors.New("请选择直连、本地代理、订阅链接或本地订阅文件")
	}
	return next, next.Validate()
}
func closeRoutes(routes []proxyroute.Route) {
	for _, r := range routes {
		r.Close()
	}
}
func routeList(routes []proxyroute.Route) []map[string]any {
	result := make([]map[string]any, 0, len(routes))
	for _, r := range routes {
		result = append(result, map[string]any{"id": r.ID, "label": r.DisplayName, "protocol": r.Protocol})
	}
	return result
}

func (c *control) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/api/environment-check" && r.Method == "GET" {
		reply(w, 200, c.environmentCheck())
		return
	}
	if r.URL.Path == "/admin/api/status" && r.Method == "GET" {
		reply(w, 200, c.status())
		return
	}
	if r.Method != "POST" {
		reply(w, 405, map[string]string{"error": "此操作需要 POST"})
		return
	}
	// One management operation at a time. Never queue a chain of test requests.
	if !c.action.TryLock() {
		reply(w, 409, map[string]string{"error": "另一个管理操作还没完成，请稍后再试"})
		return
	}
	defer c.action.Unlock()
	if !c.mu.TryLock() {
		reply(w, 409, map[string]string{"error": "Codex 正在处理请求，等这次回复结束后再操作"})
		return
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	fail := func(err error) { reply(w, 400, map[string]string{"error": err.Error()}) }
	switch r.URL.Path {
	case "/admin/api/probe-chain", "/admin/api/probe-chain/test":
		c.chainAPI(w, r, ctx)
		return
	case "/admin/api/advanced", "/admin/api/pool", "/admin/api/pool/change":
		c.poolAPI(w, r, ctx)
		return

	case "/admin/api/state-policy":
		var v struct {
			Mode string `json:"mode"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if v.Mode != "standby" && v.Mode != "on_demand" {
			fail(errors.New("请选择主备或固定主用模式"))
			return
		}
		next := c.config
		next.StateRefreshMode = v.Mode
		if err := c.persist(next); err != nil {
			fail(err)
			return
		}
		c.config = next
		if c.engine != nil {
			c.engine.SetRefreshMode(v.Mode)
		}
		reply(w, 200, map[string]string{"message": "策略已保存，现有主用/备用 state 保留，不重置冷却或上游限流。固定主用仍会在到期或检测到形状不符时重新采集。"})
	case "/admin/api/relay-models":
		var v struct {
			Base string `json:"base_url"`
			Key  string `json:"api_key"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		result, err := relayModels(ctx, v.Base, v.Key)
		if err != nil {
			fail(err)
			return
		}
		reply(w, 200, result)
	case "/admin/api/relay-repair":
		var v rebuildRequest
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			fail(errors.New("上游拒绝/限流未解除，请先处理账号状态"))
			return
		}
		if err := c.repairEndpoint(v); err != nil {
			fail(err)
			return
		}
		c.stop()
		c.setup()
		if c.setupError != "" {
			fail(errors.New(c.setupError))
			return
		}
		routes, err := proxyroute.Load(ctx, c.config)
		if err != nil {
			c.routeError = err.Error()
			fail(err)
			return
		}
		c.start(routes)
		reply(w, 200, map[string]string{"message": "缺失地址已补全并备份，其它配置保留。请重启 Codex；中转只做 Responses 转发，不采集官方 state。"})
	case "/admin/api/quick-setup":
		if err := c.quickSetup(ctx); err != nil {
			fail(err)
			return
		}
		reply(w, 200, map[string]string{"message": "本机配置已备份并接入，尚未验证外网或模型。请重启 Codex 并发一条消息；采不到合格 state 时先普通转发。"})
	case "/admin/api/state/retry":
		var v struct {
			ID      string `json:"id"`
			RouteID string `json:"route_id,omitempty"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if c.rescue || c.engine == nil || c.setupError != "" || c.routeError != "" {
			fail(errors.New("服务尚未接入，请先处理配置或出口问题"))
			return
		}
		if err := c.checkManaged(); err != nil {
			fail(errors.New("Codex 配置已改变，请先检查与修复配置"))
			return
		}
		var err error
		if v.RouteID != "" {
			err = c.engine.RetryState(ctx, v.ID, v.RouteID)
		} else {
			err = c.engine.RetryState(ctx, v.ID)
		}
		if err != nil {
			fail(err)
			return
		}
		reply(w, 200, map[string]string{"message": "本轮采集已完成。请查看会话里的实际长度和结果；采不到时不会自动重复消耗额度。"})
	case "/admin/api/codex-config/preview":
		value, err := c.cleanPreview()
		if err != nil {
			fail(err)
			return
		}
		reply(w, 200, value)
	case "/admin/api/codex-config/rebuild":
		var v rebuildRequest
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			reply(w, 409, map[string]string{"error": "上游拒绝或限流尚未解除，不能重新建立连接"})
			return
		}
		if err := c.rebuildConfig(v); err != nil {
			fail(err)
			return
		}
		c.stop() // The old engine must never resume after changing provider/auth.
		c.setup()
		if c.setupError != "" {
			fail(errors.New(c.setupError))
			return
		}
		routes, err := proxyroute.Load(ctx, c.config)
		if err != nil {
			c.routeError = err.Error()
			fail(err)
			return
		}
		c.stop()
		c.start(routes)
		if c.engine == nil {
			fail(errors.New("配置已保存，但固定出口已失效。请在路由页恢复自动选择。"))
			return
		}
		reply(w, 200, map[string]string{"message": "原 Codex 配置已完整备份，已按你选择的连接方式重建并接管。登录文件未修改。请重启 Codex；原来的插件和其他个性化设置仍在本机备份中。"})
	case "/admin/api/service-config/preview":
		value, err := c.rescuePreview()
		if err != nil {
			fail(err)
			return
		}
		reply(w, 200, value)
	case "/admin/api/service-config/reset":
		var v struct {
			SHA string `json:"expected_config_sha256"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if err := c.resetServiceConfig(v.SHA); err != nil {
			fail(err)
			return
		}
		reply(w, 200, map[string]string{"message": "原配置已备份，默认配置已生成且关闭注入。请退出此修复服务，再双击启动脚本或运行 setup。原订阅和代理信息仅在本机备份里，没有上传。"})
	case "/admin/api/recovery/preview":
		if !c.configure {
			fail(errors.New("当前为 --no-config 只读模式，请用 setup 重新启动"))
			return
		}
		preview, err := codexconfig.PreviewRecovery(c.dir)
		if err != nil {
			fail(err)
			return
		}
		message := "发现旧接管记录。建议保留当前配置；无法安全合并时，再选择恢复旧备份。"
		if !preview.Needed {
			message = "没有遗留恢复记录，可以直接点击重新检查并接管。"
		}
		if preview.Reason != "" {
			message += " " + preview.Reason
		}
		reply(w, 200, map[string]any{"message": message, "needed": preview.Needed, "can_keep_current": preview.CanKeepCurrent, "can_restore_backup": preview.CanRestoreBackup, "config_sha256": preview.ConfigSHA256, "transaction_sha256": preview.TransactionSHA256})
	case "/admin/api/recovery/apply":
		if !c.configure {
			fail(errors.New("当前为 --no-config 只读模式，未修改配置"))
			return
		}
		var v struct {
			Mode           string `json:"mode"`
			ConfigSHA      string `json:"expected_config_sha256"`
			TransactionSHA string `json:"expected_transaction_sha256"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			reply(w, 409, map[string]string{"error": "上游拒绝或限流尚未解除，不能通过重新接管重置等待状态。"})
			return
		}
		if _, err := codexconfig.Recover(c.dir, codexconfig.RecoveryOptions{Mode: v.Mode, ExpectedConfigSHA256: v.ConfigSHA, ExpectedTransactionSHA256: v.TransactionSHA}); err != nil {
			fail(err)
			return
		}
		c.managed = false
		c.stop() // The old engine must never resume after changing provider/auth.
		c.setup()
		if c.setupError != "" {
			fail(errors.New(c.setupError))
			return
		}
		routes, err := proxyroute.Load(ctx, c.config)
		if err != nil {
			c.routeError = err.Error()
			fail(err)
			return
		}
		c.stop()
		c.start(routes)
		if c.engine == nil {
			fail(errors.New("配置已保存，但固定出口已失效。请在路由页恢复自动选择。"))
			return
		}
		reply(w, 200, map[string]string{"message": "旧配置已处理，当前文件与恢复记录已独立备份，Codex 已重新接管。请重启 Codex 并新建会话。"})
	case "/admin/api/timing":
		var v timingPreferences
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if c.rescue {
			fail(errors.New("请先修复服务配置，再保存采集时间"))
			return
		}
		if err := c.applyPreferences(ctx, c.config.Model, c.config.AccountMode, c.config.StateFallback, v); err != nil {
			fail(err)
			return
		}
		reply(w, 200, map[string]string{"message": "采集设置已备份并保存，即刻生效，无需重启 Codex。旧 state 缓存已清空；后续请求可能重新采集并消耗额度。上游限流不会被重置。"})
	case "/admin/api/preferences":
		var v struct {
			Model         string `json:"model"`
			AccountMode   string `json:"account_mode"`
			StateFallback string `json:"state_fallback"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if err := c.applyPreferences(ctx, v.Model, v.AccountMode, v.StateFallback); err != nil {
			fail(err)
			return
		}
		reply(w, 200, map[string]string{"message": "设置已保存。账号规则已生效；更换默认模型后，请重启 Codex 或新建会话。服务不冒充上游没有提供的模型。"})
	case "/admin/api/diagnostics":
		reply(w, 200, map[string]any{"traffic": c.history.snapshot(), "configuration_writable": c.configure, "configured_codex": c.managed, "model": c.config.SelectedModel(), "account_mode": c.config.AccountMode, "route_count": func() any {
			if c.engine != nil {
				return c.engine.Status()["routes"]
			}
			return 0
		}(), "note": "只包含本次运行的请求类别、HTTP 状态和耗时；没有提示词、凭据、订阅地址或 state。"})
	case "/admin/api/injection":
		var v struct {
			Enabled bool `json:"enabled"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		if v.Enabled && c.effective().IsRelay() {
			fail(errors.New("当前为 API / 中转通道，不支持官方 turn-state 注入"))
			return
		}
		next := c.config
		next.InjectionDisabled = !v.Enabled
		if err := c.persist(next); err != nil {
			fail(err)
			return
		}
		c.config = next
		if c.engine != nil {
			c.engine.SetInjection(v.Enabled)
		}
		reply(w, 200, map[string]string{"message": "已保存。只影响之后的请求；关闭注入不会恢复 Codex 配置，仍通过此服务转发。"})
	case "/admin/api/sources/test", "/admin/api/sources/apply":
		var v sourceRequest
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		next, err := c.candidate(v)
		if err != nil {
			fail(err)
			return
		}
		routes, err := proxyroute.Load(ctx, next)
		if err != nil {
			fail(err)
			return
		}
		if r.URL.Path == "/admin/api/sources/test" {
			defer closeRoutes(routes)
			reply(w, 200, map[string]any{"message": "订阅获取、解析和出站配置构建通过。尚未连接出口，也没有发送模型请求。", "routes": routeList(routes)})
			return
		}
		// Appending sources can still remove a previously selected node when a
		// remote subscription changed. Never publish a broken fixed route.
		for _, id := range []string{next.PinnedRoute, next.EgressRoute} {
			if id == "" {
				continue
			}
			found := false
			for _, route := range routes {
				if route.ID == id {
					found = true
					break
				}
			}
			if !found {
				closeRoutes(routes)
				fail(errors.New("固定出口已不在订阅中，未保存新配置。请先选择其他出口或恢复自动选择"))
				return
			}
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			closeRoutes(routes)
			reply(w, 409, map[string]string{"error": "上游登录、权限或限流拦截尚未解除。不能通过换出口继续请求。"})
			return
		}
		if err = c.persist(next); err != nil {
			closeRoutes(routes)
			fail(err)
			return
		}
		c.stop()
		c.config = next
		c.start(routes)
		if c.engine == nil {
			fail(errors.New("配置已保存，但固定出口已失效。请在路由页恢复自动选择。"))
			return
		}
		reply(w, 200, map[string]string{"message": "出口已应用，不需要重启服务。旧 state 已清空，下次 Codex 请求会按新出口重新采集。"})
	case "/admin/api/routes", "/admin/api/routes/test", "/admin/api/routes/pin":
		if c.config.ChainEnabled() && r.URL.Path == "/admin/api/routes/pin" {
			fail(errors.New("采集链式模式已固定本地代理，请先关闭该模式再切换旧版路由"))
			return
		}
		var v struct {
			ID string `json:"id"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(err)
			return
		}
		routes, err := proxyroute.Load(ctx, c.config)
		if err != nil {
			fail(err)
			return
		}
		if r.URL.Path == "/admin/api/routes" {
			defer closeRoutes(routes)
			reply(w, 200, map[string]any{"routes": routeList(routes), "pinned_route": c.config.PinnedRoute})
			return
		}
		index := -1
		for i, route := range routes {
			if route.ID == v.ID {
				index = i
			}
		}
		if index < 0 && !(r.URL.Path == "/admin/api/routes/pin" && v.ID == "") {
			closeRoutes(routes)
			fail(errors.New("出口不存在，请重新加载路由列表"))
			return
		}
		if r.URL.Path == "/admin/api/routes/test" {
			defer closeRoutes(routes)
			// No bearer, no auth file, no /responses call. An HTTP response only proves
			// transport reachability; even HTTP 200 is not evidence of a usable state.
			probeCtx, stop := context.WithTimeout(ctx, 12*time.Second)
			defer stop()
			req, _ := http.NewRequestWithContext(probeCtx, "GET", c.effective().Upstream+"/models", nil)
			client := &http.Client{Transport: routes[index].Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				fail(errors.New("出口连接失败。请确认代理程序已启动、端口正确，或换一个可用节点。"))
				return
			}
			resp.Body.Close()
			reply(w, 200, map[string]any{"message": "已收到 HTTP 响应。测试未携带登录信息，401/403 不代表你的账号被拒绝；这不证明能采到符合账号规则的 state，也不证明模型可用。", "status": resp.StatusCode, "duration_ms": time.Since(start).Milliseconds()})
			return
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			closeRoutes(routes)
			reply(w, 409, map[string]string{"error": "上游拦截尚未解除，不能切换出口。"})
			return
		}
		next := c.config
		next.PinnedRoute = v.ID
		// This legacy action pins the whole route set. Mixing it with an
		// independent random/fixed exit would leave no matching candidate.
		next.EgressMode, next.EgressRoute = "state", ""
		if err = c.persist(next); err != nil {
			closeRoutes(routes)
			fail(err)
			return
		}
		c.stop()
		c.config = next
		c.start(routes)
		if c.engine == nil {
			fail(errors.New("配置已保存，但固定出口已失效。请在路由页恢复自动选择。"))
			return
		}
		reply(w, 200, map[string]string{"message": "路由已切换为采集同出口模式，旧 state 已清空。要分开采集与正式出口，请使用连接设置里的独立出口设置。"})
	case "/admin/api/recover":
		if !c.configure {
			fail(errors.New("当前使用 --no-config 只读启动。请停止服务后使用 setup 启动，再接管配置；本次没有修改任何文件。"))
			return
		}
		c.pause()
		defer c.resume()
		if c.engine != nil && c.engine.Restricted() {
			reply(w, 409, map[string]string{"error": "当前账号仍处于上游拒绝或限流状态，请先处理登录或等待。"})
			return
		}
		c.stop() // The old engine must never resume after changing provider/auth.
		c.setup()
		if c.setupError != "" {
			fail(errors.New(c.setupError))
			return
		}
		routes, err := proxyroute.Load(ctx, c.config)
		if err != nil {
			c.routeError = err.Error()
			fail(err)
			return
		}
		c.stop()
		c.start(routes)
		if c.engine == nil {
			fail(errors.New("配置已保存，但固定出口已失效。请在路由页恢复自动选择。"))
			return
		}
		reply(w, 200, map[string]string{"message": "配置检查完成。接管配置后请重启 Codex。"})
	default:
		reply(w, 404, map[string]string{"error": "没有这个管理接口"})
	}
}
