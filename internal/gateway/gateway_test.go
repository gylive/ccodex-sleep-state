package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

var testTokenTime = time.Now()

func fakeToken(blocks int, marker byte) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	raw[9] = marker
	binary.BigEndian.PutUint64(raw[1:9], uint64(testTokenTime.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}
func testEngine(t *testing.T, handler http.Handler) (*Engine, *bytes.Buffer) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	c := settings.Default()
	c.Upstream = upstream.URL + "/backend-api/codex"
	c.ProbeSeconds = 3
	logs := new(bytes.Buffer)
	routes := []proxyroute.Route{{ID: "test-route", Transport: &http.Transport{Proxy: nil}}}
	e := New(c, routes, slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(e.Close)
	return e, logs
}
func request(body, auth string) *http.Request {
	r := httptest.NewRequest("POST", "http://127.0.0.1/backend-api/codex/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+auth)
	r.Header.Set("Content-Type", "application/json")
	return r
}

const generation = `{"model":"gpt-6-astra","input":"private prompt","previous_response_id":"keep-this-chain","store":false,"stream":true}`

func complete(w http.ResponseWriter, token string) {
	w.Header().Set(turnstate.Header, token)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
}

func TestBootstrapInjectAndPreserveBody(t *testing.T) {
	token := fakeToken(10, 1)
	var probes, generations atomic.Int32
	e, logs := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
			if bytes.Contains(body, []byte("private prompt")) {
				t.Error("probe contained conversation")
			}
			var probe map[string]any
			if json.Unmarshal(body, &probe) != nil || probe["parallel_tool_calls"] != true || probe["reasoning"] != nil {
				t.Error("probe changed the protocol or forced a reasoning effort")
			}
			include, ok := probe["include"].([]any)
			if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
				t.Error("missing Codex response include field")
			}
			complete(w, token)
			return
		}
		generations.Add(1)
		if r.Header.Get(turnstate.Header) != token {
			t.Error("wrong injected state")
		}
		if string(body) != generation {
			t.Error("request body or session chain changed")
		}
		complete(w, fakeToken(10, 2))
	}))
	req := request(generation, "synthetic-account-token")
	req.Header.Set(turnstate.Header, "untrusted-client-state")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 200 || probes.Load() != 1 || generations.Load() != 1 {
		t.Fatalf("status=%d probes=%d generations=%d", w.Code, probes.Load(), generations.Load())
	}
	e.mu.Lock()
	for _, s := range e.sessions {
		active, _ := s.state.Acquire(time.Now())
		if active.Token.Value != token {
			t.Error("response replaced active state")
		}
	}
	e.mu.Unlock()
	for _, secret := range []string{"synthetic-account-token", "private prompt", "keep-this-chain", token, "untrusted-client-state"} {
		if strings.Contains(logs.String(), secret) {
			t.Error("sensitive content logged")
		}
	}
}
func TestQuotaFailureStopsProbeRoundAndCooldown(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(status) }))
			e.routes = append(e.routes, e.routes[0], e.routes[0])
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, request(generation, "synthetic-account-token"))
				if w.Code != status {
					t.Fatalf("status=%d", w.Code)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("quota/auth rejection retried %d times", calls.Load())
			}
		})
	}
}
func TestShapeRejectionNeverReplaysGeneration(t *testing.T) {
	var generated atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(turnstate.Header) == "" {
			complete(w, fakeToken(10, 1))
			return
		}
		generated.Add(1)
		complete(w, fakeToken(11, 2))
	}))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "synthetic-account-token"))
	if w.Code != 503 || generated.Load() != 1 || !strings.Contains(w.Body.String(), "state_shape_changed") {
		t.Fatal("shape policy or no-replay guarantee failed")
	}
}
func TestCredentialIsolation(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marker := byte(1)
		if strings.Contains(r.Header.Get("Authorization"), "account-two") {
			marker = 2
		}
		expected := fakeToken(10, marker)
		if v := r.Header.Get(turnstate.Header); v != "" && v != expected {
			t.Error("state crossed credential boundary")
		}
		complete(w, expected)
	}))
	for _, auth := range []string{"synthetic-account-one", "synthetic-account-two", "synthetic-account-one"} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, request(generation, auth))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if len(e.sessions) != 2 {
		t.Fatal("credentials not separated")
	}
}
func TestUnsupportedRequestsDoNotReachUpstream(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	for _, body := range []string{`{"model":"not supported"}`, `{"model":""}`, `{"input":"missing model"}`, `broken`} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, request(body, "synthetic-account-token"))
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	req := request(generation, "synthetic-account-token")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 426 {
		t.Fatal(w.Code)
	}
	req = request(generation, "synthetic-account-token")
	req.URL.Path = "/backend-api/codex/not-supported"
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("unsupported request reached network")
	}
}
func TestConcurrentBootstrapIsSingleFlight(t *testing.T) {
	var probes atomic.Int32
	// JSON logging is concurrency-safe only when its Writer is; use io.Discard
	// here rather than the sequential-test bytes.Buffer helper.
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
		}
		complete(w, fakeToken(10, 1))
	}))
	e.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "synthetic-account-token"))
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if probes.Load() != 1 {
		t.Fatalf("probes=%d, expected single-flight", probes.Load())
	}
}
func TestSSEFlushAndClientCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(turnstate.Header) == "" {
			complete(w, fakeToken(10, 1))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(turnstate.Header, fakeToken(10, 1))
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	e.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	proxy := httptest.NewServer(e)
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/backend-api/codex/responses", strings.NewReader(generation))
	req.Header.Set("Authorization", "Bearer synthetic-account-token")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatal("stream did not flush before completion")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
}
func TestLocalBoundaryAndSafeStatus(t *testing.T) {
	h := ProtectLocal("127.0.0.1:17841", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		host, origin string
		want         int
	}{{"127.0.0.1:17841", "", 204}, {"evil.invalid:17841", "", 403}, {"127.0.0.1:17841", "https://evil.invalid", 403}} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(w.Code)
		}
	}
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, fakeToken(10, 1)) }))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "synthetic-account-token"))
	data, _ := json.Marshal(e.Status())
	if bytes.Contains(data, []byte("synthetic")) || bytes.Contains(data, []byte("gAAAA")) {
		t.Fatal("status leaked credentials or state")
	}
}

func TestAuthRejectionBlocksUntilNewCredential(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) }))
	e.ServeHTTP(httptest.NewRecorder(), request(generation, "synthetic-account-token"))
	e.mu.Lock()
	for _, s := range e.sessions {
		s.mu.Lock()
		s.nextProbe = time.Time{}
		s.mu.Unlock()
	}
	e.mu.Unlock()
	e.ServeHTTP(httptest.NewRecorder(), request(generation, "synthetic-account-token"))
	if calls.Load() != 1 {
		t.Fatal("blocked credential was probed again")
	}
	e.ServeHTTP(httptest.NewRecorder(), request(generation, "synthetic-refreshed-token"))
	if calls.Load() != 2 {
		t.Fatal("refreshed credential was not admitted")
	}
}
func TestModelsDoNotActivateCollection(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[]}`) }))
	req := request("", "synthetic-account-token")
	req.Method = "GET"
	req.URL.Path = "/backend-api/codex/models"
	e.ServeHTTP(httptest.NewRecorder(), req)
	for _, s := range e.sessions {
		if s.activated {
			t.Fatal("model listing enabled background model probes")
		}
	}
}
func TestRetryAfter(t *testing.T) {
	if retryDelay("3600") != time.Hour {
		t.Fatal("numeric Retry-After ignored")
	}
	if retryDelay(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)) < 59*time.Minute {
		t.Fatal("date Retry-After ignored")
	}
	if retryDelay("invalid") != 0 {
		t.Fatal("invalid delay accepted")
	}
}

// The live upstream may issue a different envelope from our default heuristic.
// Keep the default strict, but make the reason visible without logging secrets.
func TestDifferentProbeShapeIsExplainedNotSilentlyAccepted(t *testing.T) {
	token := fakeToken(11, 1)
	var calls atomic.Int32
	e, logs := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		complete(w, token)
	}))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "synthetic-account-token"))
	if w.Code != 503 || calls.Load() != 1 {
		t.Fatal("unexpected shape must not reach generation")
	}
	var event struct {
		Result         string `json:"result"`
		StateBlocks    int    `json:"state_blocks"`
		ExpectedBlocks int    `json:"expected_blocks"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	if event.Result != "shape_mismatch" || event.StateBlocks != 11 || event.ExpectedBlocks != 10 {
		t.Fatalf("missing diagnostic: %+v", event)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "synthetic-account-token") {
		t.Fatal("probe diagnostic leaked a secret")
	}
}

func TestExplicitBaselineInjectsAndAllowsMissingResponseState(t *testing.T) {
	token := fakeToken(11, 1)
	var injected atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(turnstate.Header) == "" {
			complete(w, token)
			return
		}
		if r.Header.Get(turnstate.Header) != token {
			t.Error("state changed between collection and injection")
		}
		injected.Add(1)
		complete(w, "")
	}))
	e.config.BaselineBlocks = 11
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, request(generation, "synthetic-account-token"))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if injected.Load() != 2 {
		t.Fatal("missing response header must not discard the active state")
	}
}

func TestBootstrapDoesNotWaitForBackupRoutes(t *testing.T) {
	var calls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		complete(w, fakeToken(10, 1))
	}))
	e.routes = append(e.routes, e.routes[0], e.routes[0])
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "synthetic-account-token"))
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("first request must need only one probe plus generation: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestGenerationRejectionPausesRequestsAndProbes(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get(turnstate.Header) == "" {
					complete(w, fakeToken(10, 1))
					return
				}
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(status)
			}))
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, request(generation, "synthetic-account-token"))
				if w.Code != status {
					t.Fatalf("got %d want %d", w.Code, status)
				}
				if status == 429 && w.Header().Get("Retry-After") == "" {
					t.Fatal("missing retry delay")
				}
			}
			for _, s := range e.sessions {
				e.refresh(context.Background(), s, false)
			}
			if calls.Load() != 2 {
				t.Fatal("request or probe escaped upstream rejection")
			}
		})
	}
}

func TestDefaultBaselineSkipsMismatchedRouteAndBindsGoodRoute(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		if r.Header.Get(turnstate.Header) != "" {
			t.Error("generation used the rejected route")
		}
		complete(w, fakeToken(11, 1))
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		if v := r.Header.Get(turnstate.Header); v != "" && v != fakeToken(10, 2) {
			t.Error("wrong state injected on good route")
		}
		complete(w, fakeToken(10, 2))
	}))
	defer good.Close()
	// The second transport dials our second synthetic egress while preserving
	// the upstream URL. This tests actual route selection rather than a counter.
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(good.URL, "http://"))
	}}
	e.routes = append(e.routes, proxyroute.Route{ID: "good-route", Transport: tr})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "synthetic-account-token"))
	if w.Code != 200 || badCalls.Load() != 1 || goodCalls.Load() != 2 {
		t.Fatalf("wrong selection: status=%d bad=%d good=%d", w.Code, badCalls.Load(), goodCalls.Load())
	}
}
