package gateway

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

func TestExpandedStateCollectionAndReuse(t *testing.T) {
	for _, plan := range []string{"plus", "team", "business"} {
		for _, model := range settings.SupportedModels() {
			t.Run(plan+"/"+model, func(t *testing.T) {
				token := fakeToken(33, 90)
				var probes, generations atomic.Int32
				e, logs := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if injected := r.Header.Get(turnstate.Header); injected == "" {
						probes.Add(1)
					} else {
						generations.Add(1)
						if injected != token {
							t.Error("wrong injected state")
						}
					}
					complete(w, token)
				}))
				for range 2 {
					r := request(strings.ReplaceAll(generation, settings.Model, model), "ignored")
					r.Header = planHeader(plan, "expanded-account")
					w := httptest.NewRecorder()
					e.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				}
				if probes.Load() != 1 || generations.Load() != 2 {
					t.Fatalf("probes=%d generations=%d", probes.Load(), generations.Load())
				}
				status, _ := json.Marshal(e.Status())
				for _, want := range []string{`"usable":true`, `"observed_length":780`, `"diagnostic":"accepted"`} {
					if !strings.Contains(string(status), want) {
						t.Fatalf("missing %s in status: %s", want, status)
					}
				}
				for _, output := range []string{string(status), logs.String()} {
					if strings.Contains(output, token) {
						t.Fatal("state leaked into diagnostics")
					}
				}
			})
		}
	}
}

func TestAccountAcceptedShapes(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		baseline int
		want     []int
	}{
		{"auto", 10, []int{10, 33}},
		{"personal", 10, []int{10, 33}},
		{"team", 10, []int{12, 33}},
		{"auto", 11, []int{11}},
		{"auto", 12, []int{12}},
		{"auto", 33, []int{33}},
	} {
		c := settings.Default()
		c.AccountMode, c.BaselineBlocks = tc.mode, tc.baseline
		p := policyFor(c, http.Header{})
		if !slices.Equal(p.AcceptedBlocks, tc.want) {
			t.Fatalf("mode=%s baseline=%d policy=%+v", tc.mode, tc.baseline, p)
		}
		for i, blocks := range tc.want {
			if p.AcceptedLengths[i] != len(fakeToken(blocks, 1)) {
				t.Fatalf("wrong advertised length: %+v", p)
			}
		}
	}
}

func TestExpandedStateTimeDiagnostic(t *testing.T) {
	for _, age := range []time.Duration{-time.Minute, 2 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			raw, _ := base64.URLEncoding.DecodeString(fakeToken(33, 1))
			binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Add(-age).Unix()))
			token := base64.URLEncoding.EncodeToString(raw)
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { complete(w, token) }))
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "expired-expanded-account"))
			status, _ := json.Marshal(e.Status())
			if w.Code != 503 || !strings.Contains(string(status), `"diagnostic":"state_time_rejected"`) {
				t.Fatalf("status=%d diagnostics=%s", w.Code, status)
			}
		})
	}
}

func TestLegacyProbeAcceptsExpandedResponse(t *testing.T) {
	for _, baseline := range []int{10, 12} {
		t.Run(fmt.Sprint(baseline), func(t *testing.T) {
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blocks := 33
				if r.Header.Get(turnstate.Header) == "" {
					blocks = baseline
				}
				complete(w, fakeToken(blocks, 1))
			}))
			if baseline == 12 {
				e.config.AccountMode = "team"
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, request(generation, "legacy-to-expanded"))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("expanded response blocked: status=%d body=%s", w.Code, w.Body.String())
			}
			for _, s := range e.sessions {
				if s.state.Status(time.Now()).Strikes != 0 {
					t.Fatal("expanded response added a strike")
				}
			}
		})
	}
}
