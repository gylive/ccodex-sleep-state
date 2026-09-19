package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

const maxRequestBytes = 16 << 20

var errShape = errors.New("upstream state outside configured baseline")

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		fail(w, http.StatusUpgradeRequired, "http_sse_required", "This provider uses HTTP/SSE, not WebSocket.")
		return
	}
	compact := r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/responses/compact"
	bridgeCompact := compact && !e.config.IsRelay()
	generation := r.Method == http.MethodPost && (r.URL.Path == "/backend-api/codex/responses" || r.URL.Path == "/backend-api/codex/responses/compact")
	passthrough := (r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/models") || (r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/alpha/search")
	if !generation && !passthrough {
		fail(w, http.StatusNotFound, "unsupported_endpoint", "Endpoint is not exposed by this service.")
		return
	}
	e.requests.Add(1)
	e.lastRequest.Store(time.Now().Unix())
	if e.config.IsRelay() && officialCredential(r.Header.Get("Authorization")) {
		fail(w, http.StatusUnauthorized, "official_credentials_on_relay", "检测到官方登录凭据，已阻止发送给中转站。请为当前中转配置独立 API key。")
		return
	}
	if len(e.routes) == 0 {
		fail(w, 503, "no_routes", "没有可用出口。请在管理面板添加本地代理、订阅或启用直连。")
		return
	}
	model := e.config.SelectedModel()
	if r.Method == http.MethodPost {
		select {
		case e.bodySlots <- struct{}{}:
			defer func() { <-e.bodySlots }()
		case <-r.Context().Done():
			fail(w, 503, "local_request_capacity", "本机请求等待已取消；请求尚未转发")
			return
		}

		body, status, err := requestBodyWithLimits(r, e.config.RequestBytes(), e.config.WindowBytes())
		if err != nil {
			code := "invalid_request_encoding"
			if status == http.StatusUnsupportedMediaType {
				code = "unsupported_encoding"
			}
			if status == http.StatusRequestEntityTooLarge {
				code = "request_too_large"
			}
			fail(w, status, code, err.Error())
			return
		}
		if generation {
			var input struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(body, &input) != nil {
				fail(w, 400, "invalid_json", "Expected a JSON request.")
				return
			}
			if !settings.SupportedModel(input.Model) {
				fail(w, 400, "unsupported_model", "支持 gpt-6-astra、gpt-5.6-sol 和 gpt-5.6-terra；请在 Codex 中选择受支持的模型。")
				return
			}
			model = input.Model
			if !compact {
				compact = remoteCompactionV2(body)
			}
		}
		if bridgeCompact {
			body, err = bridgeCompactRequestLimit(body, e.config.RequestBytes())
			if err != nil {
				fail(w, 400, "invalid_compaction_request", err.Error())
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
		r.Header.Del("Transfer-Encoding")
		r.TransferEncoding = nil
	}
	s, err := e.borrow(r.Header, model)
	if err != nil {
		fail(w, http.StatusUnauthorized, "authentication_required", "Log in with Codex before using this service.")
		return
	}
	defer release(s)
	if rejectRequest(w, s) {
		return
	}
	// Compaction has its own upstream protocol. Do not require a synthetic
	// generation probe or apply response-state shape rules to its result.
	inject := generation && !compact && !e.disabled.Load()
	if inject {
		s.mu.Lock()
		s.activated = true
		s.mu.Unlock()
	}
	snapshot, usable := s.state.Acquire(time.Now())
	if inject && !usable {
		e.refresh(r.Context(), s, true)
		snapshot, usable = s.state.Acquire(time.Now())
	}
	if rejectRequest(w, s) {
		return
	}
	if inject && !usable && e.config.StateFallback == "passthrough" {
		// The user's generation has not been sent yet. Forward it once without
		// injection; never replay it after an upstream response or account limit.
		inject = false
		w.Header().Set("X-Sleep-State-Mode", "fallback-passthrough")
	}
	if inject && !usable {
		message, seconds := s.unavailableMessage()
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		fail(w, 503, "state_unavailable", message)
		return
	}
	route := 0
	if e.config.PinnedRoute != "" {
		found := false
		for i := range e.routes {
			if e.routes[i].ID == e.config.PinnedRoute {
				route, found = i, true
				break
			}
		}
		if !found {
			fail(w, 503, "pinned_route_unavailable", "指定出口不存在，请在路由页面重新选择。")
			return
		}
	}
	if (inject || compact) && usable {
		route = snapshot.Route
	}
	if e.config.EgressMode == "random" || e.config.EgressMode == "fixed" {
		selected, err := e.selectEgress(snapshot.Route, (inject || compact) && usable)
		if err != nil {
			fail(w, 503, "egress_unavailable", err.Error())
			return
		}
		route = selected
	}
	// Manual removal must apply even in legacy cycling mode and to compact/
	// metadata requests, not just to new state probes.
	// In probe-only chain mode, disabled landing nodes stop collection on that
	// node, not ordinary traffic through the independent local first hop.
	if e.routes[route].ProbeTransport == nil && e.pool.Get(e.routes[route].ID).State == "disabled" {
		fail(w, 503, "pool_node_disabled", "所选出口已停用，请在代理池手动放回或选择其他出口")
		return
	}
	if e.config.PoolEnabled && generation {
		// Reserve before dispatch so concurrent requests cannot consume one random
		// node twice. A fixed user exit is explicitly reusable.
		allowUsed := e.config.EgressMode != "random"
		if err := e.pool.Claim(e.routes[route].ID, allowUsed); err != nil {
			fail(w, 503, "pool_node_unavailable", err.Error())
			return
		}
	}
	target, _ := url.Parse(e.config.Upstream)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, "/backend-api/codex")
			if bridgeCompact {
				pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + "/responses"
				pr.Out.Header.Set("Accept", "text/event-stream")
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			if e.config.IsRelay() {
				pr.Out.Header.Del(turnstate.Header)
			}
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Proxy-Authorization")
			if e.config.IsRelay() {
				for key := range pr.Out.Header {
					if strings.HasPrefix(strings.ToLower(key), "chatgpt-") {
						pr.Out.Header.Del(key)
					}
				}
				pr.Out.Header.Del("Originator")
				pr.Out.Header.Del("Version")
			}
			if inject {
				pr.Out.Header.Set(turnstate.Header, snapshot.Token.Value)
			}
		},
		Transport: e.routes[route].Transport, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if e.config.IsRelay() {
				resp.Header.Del(turnstate.Header)
			}
			if resp.StatusCode == 503 {
				resp.Header.Set("X-Sleep-State-Error-Source", "upstream")
			}
			e.reject(s, resp.StatusCode, retryDelay(resp.Header.Get("Retry-After")), route)
			if bridgeCompact && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return e.bridgeCompactResponse(resp, s, route)
			}
			if inject && resp.StatusCode >= 200 && resp.StatusCode < 300 && s.state.Observe(resp.Header.Get(turnstate.Header), snapshot, time.Now()) {
				// Never replay a generation request: it may already have run upstream.
				resp.Body.Close()
				s.state.RejectAndPromote(snapshot, time.Now())
				return errShape
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var compactFailure *compactError
			if errors.As(err, &compactFailure) {
				if compactFailure.status == 429 {
					_, seconds := s.rejection()
					w.Header().Set("Retry-After", strconv.Itoa(seconds))
				}
				fail(w, compactFailure.status, compactFailure.code, compactFailure.message)
				return
			}
			if errors.Is(err, errShape) {
				fail(w, 503, "state_shape_changed", "响应头 state 不符合规则，正文已拦截；已尝试切换备用 state，供下一次请求使用。本次可能已计费，不自动重放，请查看主备状态。")
				return
			}
			fail(w, 502, "upstream_unavailable", "Upstream connection failed. Request was not replayed.")
		},
	}
	started := time.Now()
	tracked := &statusWriter{ResponseWriter: w, status: 200}
	defer func() {
		e.log.Info("request_finished", "status", tracked.status, "duration_ms", time.Since(started).Milliseconds(), "route", e.routes[route].ID, "state_version", snapshot.Version, "model", s.model, "compaction", compact)
	}()
	proxy.ServeHTTP(tracked, r)
	if e.config.PoolEnabled && generation {
		st, reason := "used", "request_dispatched"
		if tracked.status >= 400 {
			st, reason = "failed", "request_failed"
		}
		_ = e.pool.Change([]string{e.routes[route].ID}, st, reason, false)
	}
}
func rejectRequest(w http.ResponseWriter, s *session) bool {
	status, seconds := s.rejection()
	if status == 0 {
		return false
	}
	if status == 429 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		fail(w, status, "upstream_rate_limited", "Upstream requested a pause. No request or probe was sent; wait before retrying.")
	} else {
		fail(w, status, "upstream_auth_rejected", "Upstream rejected these credentials. Resolve login or access in Codex before retrying.")
	}
	return true
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("X-Sleep-State-Error-Source", "local")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "sleep_state_error", "code": code, "message": message}})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *statusWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ProtectLocal rejects browser-origin requests and DNS-rebinding hostnames.
// Management endpoints additionally require a separate random local token.
func ProtectLocal(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, host) || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			fail(w, 403, "local_clients_only", "Browser and non-loopback host requests are not accepted.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// An OAuth access token must never be mistaken for a relay API key. Claims here
// are only a fail-closed leak check, not authentication or signature validation.
func officialCredential(auth string) bool {
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	if _, ok := claims["https://api.openai.com/auth"]; ok {
		return true
	}
	var issuer string
	_ = json.Unmarshal(claims["iss"], &issuer)
	u, err := url.Parse(issuer)
	return err == nil && (u.Hostname() == "auth.openai.com" || u.Hostname() == "auth0.openai.com")
}

// Remote compaction v2 uses /responses with a final protocol item rather than
// /responses/compact. Match the actual item, not text that mentions its name.
func remoteCompactionV2(body []byte) bool {
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Input) == 0 {
		return false
	}
	var last map[string]json.RawMessage
	if json.Unmarshal(request.Input[len(request.Input)-1], &last) != nil || len(last) != 1 {
		return false
	}
	var kind string
	return json.Unmarshal(last["type"], &kind) == nil && kind == "compaction_trigger"
}
