package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

func planHeader(plan, account string) http.Header {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": plan, "chatgpt_account_id": account}})
	h := make(http.Header)
	h.Set("Authorization", "Bearer header."+base64.RawURLEncoding.EncodeToString(payload)+".signature")
	h.Set("ChatGPT-Account-Id", account)
	return h
}

func TestAccountPolicies(t *testing.T) {
	cases := []struct {
		name, mode, plan, selected, wantMode, source string
		baseline, blocks, length                     int
	}{
		{"unknown uses explicit fallback", "auto", "", "", "personal", "default", 10, 10, 292},
		{"plus", "auto", "plus", "account", "personal", "token_hint", 10, 10, 292},
		{"team", "auto", "team", "account", "team", "token_hint", 10, 12, 332},
		{"business", "auto", "business", "account", "team", "token_hint", 10, 12, 332},
		{"different workspace no guess", "auto", "plus", "other", "personal", "default", 10, 10, 292},
		{"unknown enterprise no guess", "auto", "enterprise", "account", "personal", "default", 10, 10, 292},
		{"manual team wins", "team", "plus", "account", "team", "manual", 10, 12, 332},
		{"manual personal wins", "personal", "team", "account", "personal", "manual", 12, 10, 292},
		{"custom baseline migration", "auto", "team", "account", "custom", "legacy_baseline", 11, 11, 312},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := settings.Default()
			c.AccountMode = tt.mode
			c.BaselineBlocks = tt.baseline
			h := planHeader(tt.plan, "account")
			h.Set("ChatGPT-Account-Id", tt.selected)
			p := policyFor(c, h)
			if p.Mode != tt.wantMode || p.Source != tt.source || p.Blocks != tt.blocks || p.Length != tt.length {
				t.Fatalf("policy = %+v", p)
			}
			if !strings.Contains(p.Note, "规则") {
				t.Fatal("missing heuristic explanation")
			}
		})
	}
}

func TestModelsKeepIndependentStateAndProbeTheirOwnModel(t *testing.T) {
	tokens := map[string]string{}
	probes := map[string]int{}
	for i, m := range settings.SupportedModels() {
		tokens[m] = fakeToken(10, byte(20+i))
	}
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		expected, ok := tokens[body.Model]
		if !ok {
			t.Errorf("unexpected model %q", body.Model)
			w.WriteHeader(400)
			return
		}
		if injected := r.Header.Get(turnstate.Header); injected == "" {
			probes[body.Model]++
		} else if injected != expected {
			t.Errorf("state crossed models: %s", body.Model)
		}
		complete(w, expected)
	}))
	for round := 0; round < 2; round++ {
		for _, m := range settings.SupportedModels() {
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(strings.ReplaceAll(generation, settings.Model, m), "shared-account-token"))
			if w.Code != 200 {
				t.Fatalf("model %s returned %d: %s", m, w.Code, w.Body.String())
			}
		}
	}
	if len(e.sessions) != 3 {
		t.Fatalf("sessions=%d", len(e.sessions))
	}
	for model, n := range probes {
		if n != 1 {
			t.Errorf("model %s probes=%d", model, n)
		}
	}
	status, _ := json.Marshal(e.Status())
	if strings.Contains(string(status), "shared-account-token") {
		t.Fatal("credentials leaked into status")
	}
	for _, token := range tokens {
		if strings.Contains(string(status), token) {
			t.Fatal("state leaked into status")
		}
	}
}

func TestTeamAccepts332AndRejects356(t *testing.T) {
	for _, blocks := range []int{12, 13} {
		t.Run(string(rune('a'+blocks)), func(t *testing.T) {
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, fakeToken(blocks, 40)) }))
			r := request(generation, "ignored-token")
			r.Header = planHeader("team", "team-account")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			want := 200
			if blocks == 13 {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("blocks=%d status=%d: %s", blocks, w.Code, w.Body.String())
			}
			encoded, _ := json.Marshal(e.Status())
			if !strings.Contains(string(encoded), `"expected_length":332`) {
				t.Fatalf("missing team policy: %s", encoded)
			}
			if blocks == 13 && !strings.Contains(string(encoded), `"observed_length":356`) {
				t.Fatal("missing rejected shape diagnostic")
			}
		})
	}
}

func TestRejectionSurvivesModelSwitch(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(code) }))
			for _, m := range settings.SupportedModels() {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, request(strings.ReplaceAll(generation, settings.Model, m), "same-rejected-account"))
				if w.Code != code {
					t.Fatalf("model=%s status=%d", m, w.Code)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("switched models retried rejected credentials %d times", calls.Load())
			}
		})
	}
}

func TestCompactionDoesNotDependOnCollection(t *testing.T) {
	var calls atomic.Int32
	body := `{"model":"gpt-5.6-terra","input":[],"instructions":"compact"}`
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Error("legacy bridge did not use V2 endpoint")
		}
		b, _ := io.ReadAll(r.Body)
		if !remoteCompactionV2(b) {
			t.Error("missing official compaction trigger")
		}
		if r.Header.Get(turnstate.Header) != "client-owned-state" {
			t.Error("compaction state header modified")
		}
		completeCompact(w, "fixture-opaque-state")
	}))
	r := request(body, "compaction-account")
	r.URL.Path = "/backend-api/codex/responses/compact"
	r.Header.Set(turnstate.Header, "client-owned-state")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "response.compaction") {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
	for _, s := range e.sessions {
		if s.activated || s.state.Status(time.Now()).Usable {
			t.Fatal("compaction started collection")
		}
	}
}

func TestProbeDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"missing", func(w http.ResponseWriter, r *http.Request) { complete(w, "") }, "missing_state_header"},
		{"invalid", func(w http.ResponseWriter, r *http.Request) { complete(w, "invalid-state") }, "invalid_state_envelope"},
		{"incomplete", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(turnstate.Header, fakeToken(10, 4))
			_, _ = io.WriteString(w, `data: {"type":"response.created"}`)
		}, "incomplete_response"},
		{"shape", func(w http.ResponseWriter, r *http.Request) { complete(w, fakeToken(11, 4)) }, "shape_mismatch"},
		{"upstream", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }, "upstream_rejected"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEngine(t, tt.handler)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "diagnostic-account"))
			if w.Code != 503 {
				t.Fatalf("status=%d", w.Code)
			}
			status, _ := json.Marshal(e.Status())
			if !strings.Contains(string(status), `"diagnostic":"`+tt.want+`"`) {
				t.Fatalf("status=%s", status)
			}
			if w.Header().Get("Retry-After") == "30" {
				t.Fatal("retry-after ignored actual cooldown")
			}
		})
	}
}

func TestCompactionKeepsStateRouteAndDoesNotInspectResponseShape(t *testing.T) {
	for _, blocks := range []int{12, 13} {
		t.Run(string(rune('a'+blocks)), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.RawQuery != "trace=a%2Fb&x=1" {
					t.Errorf("query changed: %s", r.URL.RawQuery)
				}
				w.Header().Set(turnstate.Header, fakeToken(blocks, 88))
				completeCompact(w, "fixture-opaque-state")
			}))
			activeRoute := e.routes[0]
			e.routes = []proxyroute.Route{{ID: "must-not-use", Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, errors.New("wrong route") }}}, activeRoute}
			r := request(compactFixtureRequest, "compact-affinity-token")
			r.URL.Path = "/backend-api/codex/responses/compact"
			r.URL.RawQuery = "trace=a%2Fb&x=1"
			session, err := e.borrow(r.Header)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := turnstate.Parse(fakeToken(10, 2))
			if !session.state.Offer(state, 1, time.Now()) {
				t.Fatal("seed state rejected")
			}
			release(session)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			if session.state.Status(time.Now()).Strikes != 0 {
				t.Fatal("compaction response contaminated generation state")
			}
		})
	}
}

func TestCompactionRejectionBlocksOtherModels(t *testing.T) {
	for _, code := range []int{401, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(code) }))
			r := request(compactFixtureRequest, "compact-rejected-token")
			r.URL.Path = "/backend-api/codex/responses/compact"
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != code {
				t.Fatalf("compact status=%d", w.Code)
			}
			w = httptest.NewRecorder()
			e.ServeHTTP(w, request(strings.ReplaceAll(generation, settings.Model, "gpt-5.6-sol"), "compact-rejected-token"))
			if w.Code != code || calls.Load() != 1 {
				t.Fatalf("switched model bypassed compact rejection status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestUnusableTeamShapeCannotChangePersonalPolicy(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, fakeToken(12, 5)) }))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "unknown-plan-token"))
	if w.Code != 503 {
		t.Fatal("inferred Team account from state length")
	}
	for _, s := range e.sessions {
		if s.policy.Mode != "personal" || s.policy.Source != "default" {
			t.Fatalf("policy mutated: %+v", s.policy)
		}
	}
}

func TestMetricsCountRejectedRequestsWithoutSession(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("request should fail locally") }))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(`{"model":"not supported"}`, "metric-token"))
	status := e.Status()
	if status["requests_total"] != uint64(1) || status["last_request_unix"].(int64) == 0 || len(e.sessions) != 0 {
		t.Fatalf("missing request evidence: %+v", status)
	}
}

func TestRemoteCompactionV2Recognition(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"input":[{"type":"compaction_trigger"}]}`, true},
		{`{"input":[{"type":"message","role":"user","content":"compact now"},{"type":"compaction_trigger"}]}`, true},
		{`{"input":"compaction_trigger"}`, false},
		{`{"input":[{"type":"message","content":"compaction_trigger"}]}`, false},
		{`{"input":[{"type":"compaction_trigger","extra":true}]}`, false},
		{`{"input":[{"type":"compaction_trigger"},{"type":"message"}]}`, false},
	}
	for _, tt := range cases {
		if got := remoteCompactionV2([]byte(tt.body)); got != tt.want {
			t.Errorf("%s: got %v", tt.body, got)
		}
	}
}

func TestRemoteCompactionV2PassesWithoutProbeOrShapeGate(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger"}],"stream":true}`
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		if string(data) != body || r.Header.Get(turnstate.Header) != "client-state" {
			t.Error("V2 compaction request changed")
		}
		complete(w, fakeToken(13, 9))
	}))
	r := request(body, "v2-compaction-account")
	r.Header.Set(turnstate.Header, "client-state")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestStateFallbackForwardsOriginalRequestOnce(t *testing.T) {
	for _, mode := range []string{"", "strict", "passthrough"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			var calls, generations atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				if string(data) == generation {
					generations.Add(1)
					if r.Header.Get(turnstate.Header) != "" {
						t.Error("fallback injected rejected state")
					}
				}
				complete(w, fakeToken(11, 18))
			}))
			e.config.StateFallback = mode
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "fallback-account"))
			wantStatus, wantCalls, wantGenerations := 503, int32(1), int32(0)
			if mode == "passthrough" {
				wantStatus, wantCalls, wantGenerations = 200, 2, 1
			}
			if w.Code != wantStatus || calls.Load() != wantCalls || generations.Load() != wantGenerations {
				t.Fatalf("status=%d calls=%d generations=%d", w.Code, calls.Load(), generations.Load())
			}
			if mode == "passthrough" && w.Header().Get("X-Sleep-State-Mode") != "fallback-passthrough" {
				t.Fatal("fallback was not identified")
			}
		})
	}
}

func TestStateFallbackNeverBypassesAccountRejection(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(code) }))
			e.config.StateFallback = "passthrough"
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "fallback-rejected"))
			if w.Code != code || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestManualRetryRespectsCooldownAndKeepsOpaqueSessionID(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); complete(w, fakeToken(11, 31)) }))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "manual-retry-token"))
	var s *session
	for _, item := range e.sessions {
		s = item
	}
	if s == nil || s.id == "" {
		t.Fatal("no opaque session id")
	}
	if err := e.RetryState(context.Background(), s.id); err == nil || !strings.Contains(err.Error(), "冷却") {
		t.Fatalf("cooldown error=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("manual retry skipped cooldown")
	}
	s.mu.Lock()
	s.nextProbe = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if err := e.RetryState(context.Background(), s.id); err == nil {
		t.Fatal("bad state became accepted")
	}
	if calls.Load() != 2 {
		t.Fatalf("retry count=%d", calls.Load())
	}
	status, _ := json.Marshal(e.Status())
	if !strings.Contains(string(status), `"id":"`+s.id+`"`) {
		t.Fatal("panel cannot identify retry session")
	}
	if err := e.RetryState(context.Background(), "not-a-session"); err == nil {
		t.Fatal("unknown session accepted")
	}
}

func TestManualRetryDoesNotClearReadyStateOrAccountLimits(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); complete(w, fakeToken(10, 32)) }))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "manual-ready-token"))
	var s *session
	for _, item := range e.sessions {
		s = item
	}
	before, _ := s.state.Acquire(time.Now())
	s.mu.Lock()
	s.nextProbe = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if err := e.RetryState(context.Background(), s.id); err == nil {
		t.Fatal("ready state was unnecessarily replaced")
	}
	after, _ := s.state.Acquire(time.Now())
	if before != after || calls.Load() != 2 {
		t.Fatal("manual retry changed active state")
	}
	e.reject(s, 429, time.Minute, 0)
	if err := e.RetryState(context.Background(), s.id); err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatalf("missing account restriction: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("manual retry bypassed quota")
	}
}

func TestManualRetryReportsBusyAndSuccess(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, fakeToken(10, 33)) }))
	s, err := e.borrow(request(generation, "manual-busy-token").Header)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.RetryState(context.Background(), s.id); err == nil || !strings.Contains(err.Error(), "处理请求") {
		t.Fatalf("busy error=%v", err)
	}
	release(s)
	s.mu.Lock()
	s.activated = true
	s.mu.Unlock()
	if err = e.RetryState(context.Background(), s.id); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if _, usable := s.state.Acquire(time.Now()); !usable {
		t.Fatal("successful retry did not publish state")
	}
	e.SetInjection(false)
	if err = e.RetryState(context.Background(), s.id); err == nil {
		t.Fatal("collected while injection disabled")
	}
}

func TestBackgroundSelectionPinsCredentialGuardAcrossIdleBoundary(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("selection test must not send requests") }))
	headers := request(generation, "worker-pin-account").Header
	s, err := e.borrow(headers)
	if err != nil {
		t.Fatal(err)
	}
	release(s)
	now := time.Now()
	s.mu.Lock()
	s.lastUsed = now.Add(-idleLifetime).Add(time.Nanosecond)
	s.activated = true
	s.mu.Unlock()
	work := e.backgroundWork(now)
	if len(work) != 1 || work[0] != s {
		t.Fatal("active worker session not selected")
	}
	defer release(s)
	// Selection used the instant immediately before expiration. By the time a
	// new model borrows the same credentials, the idle boundary has passed.
	other, err := e.borrow(headers, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	defer release(other)
	if len(e.sessions) != 2 || s.limit != other.limit {
		t.Fatal("queued background work lost its shared credential guard")
	}
	e.reject(s, 429, time.Minute, 0)
	if code, _ := other.rejection(); code != 429 {
		t.Fatal("new model bypassed rejection from pinned background probe")
	}
	if next := e.backgroundWork(time.Now()); len(next) != 0 {
		t.Fatal("busy sessions were queued again")
	}
}

func TestInjectionOffPreservesClientOwnedState(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(turnstate.Header) != "client-owned-original-state" {
			t.Error("disabled injection removed or replaced client state")
		}
		complete(w, fakeToken(13, 75))
	}))
	e.SetInjection(false)
	r := request(generation, "off-client-state-account")
	r.Header.Set(turnstate.Header, "client-owned-original-state")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestStateFallbackPreservesClientOwnedStateButProbeDoesNotUseIt(t *testing.T) {
	var probes, generations atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == generation {
			generations.Add(1)
			if r.Header.Get(turnstate.Header) != "client-fallback-state" {
				t.Error("fallback changed client-owned state")
			}
		} else {
			probes.Add(1)
			if r.Header.Get(turnstate.Header) != "" {
				t.Error("probe inherited client conversation state")
			}
		}
		complete(w, fakeToken(11, 76))
	}))
	e.config.StateFallback = "passthrough"
	r := request(generation, "fallback-client-state-account")
	r.Header.Set(turnstate.Header, "client-fallback-state")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 200 || probes.Load() != 1 || generations.Load() != 1 {
		t.Fatalf("status=%d probes=%d generations=%d", w.Code, probes.Load(), generations.Load())
	}
}
