package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

func TestOnlyStateProbesUseChainTransport(t *testing.T) {
	var probes, normal atomic.Int32
	token := fakeToken(10, 78)
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normal.Add(1)
		if r.URL.Path == "/backend-api/codex/responses" {
			body, _ := io.ReadAll(r.Body)
			if string(body) == generation && r.Header.Get(turnstate.Header) != token {
				t.Error("generation did not inject chain-collected state")
			}
		}
		complete(w, token)
	}))
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "private prompt") || !strings.Contains(string(body), "Reply with OK.") || r.Header.Get(turnstate.Header) != "" {
			t.Error("chain received conversation or previous state")
		}
		complete(w, token)
	}))
	defer probe.Close()
	e.routes[0].ProbeTransport = &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, probe.Listener.Addr().String())
	}}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, request(generation, "chain-fixture-key"))
	if w.Code != 200 || probes.Load() != 1 || normal.Load() != 1 {
		t.Fatalf("bootstrap: status=%d probes=%d normal=%d", w.Code, probes.Load(), normal.Load())
	}
	// Both V2 compaction and metadata keep using the first hop alone.
	r := request(`{"model":"gpt-6-astra","input":[{"type":"compaction_trigger"}]}`, "chain-fixture-key")
	e.ServeHTTP(httptest.NewRecorder(), r)
	r = httptest.NewRequest("GET", "http://127.0.0.1/backend-api/codex/models", nil)
	r.Header.Set("Authorization", "Bearer chain-fixture-key")
	e.ServeHTTP(httptest.NewRecorder(), r)
	// A different credential would require a probe if disabling were ineffective.
	e.SetInjection(false)
	e.ServeHTTP(httptest.NewRecorder(), request(`{"model":"gpt-6-astra","input":"plain"}`, "chain-disabled-key"))
	if probes.Load() != 1 || normal.Load() != 4 {
		t.Fatalf("non-probes reached chain: probes=%d normal=%d", probes.Load(), normal.Load())
	}
}

func TestChainProbeAccountRejectionStillStopsGeneration(t *testing.T) {
	var normal, probes atomic.Int32
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { normal.Add(1) }))
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1); w.WriteHeader(403) }))
	defer probe.Close()
	e.routes[0].ProbeTransport = &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, probe.Listener.Addr().String())
	}}
	e.config.StateFallback = "passthrough"
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, request(`{"model":"`+model+`","input":"private prompt"}`, "chain-rejected-key"))
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if probes.Load() != 1 || normal.Load() != 0 {
		t.Fatal("chain bypassed upstream rejection")
	}
}

func TestProbeRefreshMovesToNextLandingExit(t *testing.T) {
	token := fakeToken(10, 91)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		complete(w, "") // complete response without a state header
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		complete(w, token)
	}))
	defer second.Close()
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, token) }))
	defer e.Close()
	transportFor := func(server *httptest.Server) *http.Transport {
		return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}}
	}
	e.routes[0].ProbeTransport = transportFor(first)
	e.routes = append(e.routes, proxyroute.Route{ID: "chain-exit-2", Transport: transportFor(second), ProbeTransport: transportFor(second)})
	r := request(generation, "chain-multi-exit-key")
	s, err := e.borrow(r.Header)
	if err != nil {
		t.Fatal(err)
	}
	defer release(s)
	e.refresh(context.Background(), s, true)
	active, usable := s.state.Acquire(time.Now())
	if !usable || active.Route != 1 || active.Token.Value != token {
		t.Fatalf("did not advance to second exit: usable=%v route=%d", usable, active.Route)
	}
}
