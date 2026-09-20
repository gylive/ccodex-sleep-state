package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/proxyroute"
	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

func TestValidRequestModel(t *testing.T) {
	for _, model := range []string{"auxiliary-review-fixture", settings.Model, strings.Repeat("a", 256)} {
		if !validRequestModel(model) {
			t.Errorf("valid model rejected: %q", model)
		}
	}
	for _, model := range []string{"", strings.Repeat("a", 257), "model name", "model\tname", "model\nname", "model\x00", "model\x7f", "model", "model ", "model ", "model"} {
		if validRequestModel(model) {
			t.Errorf("invalid model accepted: %q", model)
		}
	}
}

// auxiliaryEngine mirrors testEngine but lets a case choose the injection mode.
func auxiliaryEngine(t *testing.T, disabled bool, handler http.Handler) *Engine {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	c := settings.Default()
	c.Upstream = upstream.URL + "/backend-api/codex"
	c.ProbeSeconds = 3
	c.InjectionDisabled = disabled
	e := New(c, []proxyroute.Route{{ID: "test-route", Transport: &http.Transport{Proxy: nil}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(e.Close)
	return e
}

func TestAuxiliaryModelPassesThroughWithoutProbesOrState(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "injection on", true: "injection off"}[disabled], func(t *testing.T) {
			body := strings.ReplaceAll(generation, settings.Model, "auxiliary-review-fixture")
			var calls atomic.Int32
			var mismatch, leaked, reauthed atomic.Bool
			e := auxiliaryEngine(t, disabled, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				received, _ := io.ReadAll(r.Body)
				if !bytes.Equal(received, []byte(body)) {
					mismatch.Store(true)
				}
				if r.Header.Get(turnstate.Header) != "" {
					leaked.Store(true)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-review-key" {
					reauthed.Store(true)
				}
				w.Header().Set(turnstate.Header, "must-not-escape")
				io.WriteString(w, "upstream reviewer result")
			}))
			r := request(body, "synthetic-review-key")
			r.Header.Set(turnstate.Header, "unrelated-model-state")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if mismatch.Load() {
				t.Error("request body changed or a synthetic probe was sent")
			}
			if leaked.Load() {
				t.Error("turn state leaked to the auxiliary model")
			}
			if reauthed.Load() {
				t.Error("credential changed on the way upstream")
			}
			if w.Code != 200 || w.Body.String() != "upstream reviewer result" || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			if w.Header().Get(turnstate.Header) != "" {
				t.Error("turn state escaped to the client")
			}
			if w.Header().Get("X-Sleep-State-Mode") != "model-passthrough" {
				t.Error("passthrough was not reported to the client")
			}
			if work := e.backgroundWork(time.Now()); len(work) != 0 {
				t.Fatal("auxiliary model activated background probes")
			}
			s, err := e.borrow(r.Header, "auxiliary-review-fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer release(s)
			e.refresh(context.Background(), s, true)
			if calls.Load() != 1 {
				t.Fatal("explicit refresh probed the auxiliary model")
			}
		})
	}
}

func TestAuxiliaryModelRejectionStillBlocksCredentialAcrossModels(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			e := auxiliaryEngine(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				io.WriteString(w, "upstream refusal")
			}))
			for _, model := range []string{"auxiliary-review-fixture", settings.Model} {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, request(strings.ReplaceAll(generation, settings.Model, model), "synthetic-review-key"))
				if w.Code != status {
					t.Fatalf("model=%s status=%d want=%d", model, w.Code, status)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream request replayed or rejection bypassed: %d", calls.Load())
			}
		})
	}
}
