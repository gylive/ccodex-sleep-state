package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
)

const chainFixture = `{"enabled":true,"local_proxy":"http://127.0.0.1:7897","exit_proxy":"alice:private-pass@exit.invalid:8080","protocol":"http"}`

func TestChainPanelSaveRedactionAndDisable(t *testing.T) {
	c, h := panelControl(t, func(cfg *settings.Config) {
		cfg.EgressMode, cfg.PoolEnabled = "random", true
		cfg.ProxyURLs = []string{"http://127.0.0.1:18080"}
	})
	w := panelPost(h, "probe-chain", chainFixture)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !c.config.ChainEnabled() || c.config.PoolEnabled || c.config.EgressMode != "state" || len(c.config.ProxyURLs) != 1 || c.engine == nil {
		t.Fatal("chain did not replace active pool while preserving sources")
	}
	status := panelRequest(h, "GET", "/admin/api/status", "", map[string]string{"Authorization": "Bearer " + panelTestToken})
	pool := panelPost(h, "pool", `{}`)
	for _, output := range []string{w.Body.String(), status.Body.String(), pool.Body.String()} {
		for _, secret := range []string{"alice", "private-pass", "exit.invalid", "7897"} {
			if strings.Contains(output, secret) {
				t.Fatal("private chain address leaked")
			}
		}
	}
	loaded, err := settings.Load(c.path)
	if err != nil || !loaded.ChainEnabled() || loaded.ProbeChain.ExitProxy != "http://alice:private-pass@exit.invalid:8080" {
		t.Fatal("chain did not persist", err)
	}
	backup, err := os.ReadDir(filepath.Join(c.dir, "backups"))
	if err != nil || len(backup) == 0 {
		t.Fatal("missing old configuration backup")
	}
	// Blank password fields keep saved values and never clear them by accident.
	w = panelPost(h, "probe-chain", `{"enabled":false}`)
	if w.Code != 200 || c.config.ChainEnabled() || c.config.ProbeChain.ExitProxy != loaded.ProbeChain.ExitProxy {
		t.Fatal(w.Code)
	}
	if c.engine.Status()["routes"] != 2 {
		t.Fatal("old sources not restored")
	}
	w = panelPost(h, "probe-chain", `{"enabled":true}`)
	if w.Code != 200 || !c.config.ChainEnabled() {
		t.Fatal(w.Code)
	}
	if w = panelPost(h, "sources/apply", `{"mode":"direct"}`); w.Code != 400 {
		t.Fatal("source update silently ignored active chain")
	}
	if w = panelPost(h, "routes/pin", `{"id":""}`); w.Code != 400 {
		t.Fatal("legacy pin mixed with chain")
	}
}

func TestChainPanelRejectsInvalidAndUnauthenticatedWithoutWriting(t *testing.T) {
	c, h := panelControl(t, nil)
	before := diskConfig(t, c)
	w := panelRequest(h, "POST", "/admin/api/probe-chain", chainFixture, nil)
	if w.Code != 401 || diskConfig(t, c) != before {
		t.Fatal("unauthenticated chain update")
	}
	bad := strings.Replace(chainFixture, "127.0.0.1", "external.invalid", 1)
	w = panelPost(h, "probe-chain", bad)
	if w.Code != 400 || diskConfig(t, c) != before || strings.Contains(w.Body.String(), "private-pass") {
		t.Fatal("invalid chain update mutated or leaked")
	}
	w = panelPost(h, "probe-chain", `{"enabled":true}`)
	if w.Code != 400 || diskConfig(t, c) != before {
		t.Fatal("missing addresses accepted")
	}
}

func TestChainReconfigurationPreservesAccountStop(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(403) }))
	defer server.Close()
	c, h := panelControl(t, func(cfg *settings.Config) {
		cfg.Upstream = server.URL + "/backend-api/codex"
		cfg.InjectionDisabled = true
	})
	r := httptest.NewRequest("POST", "http://127.0.0.1/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"synthetic"}`))
	r.Header.Set("Authorization", "Bearer synthetic-chain-denied")
	c.engine.ServeHTTP(httptest.NewRecorder(), r)
	before := diskConfig(t, c)
	w := panelPost(h, "probe-chain", chainFixture)
	if w.Code != 400 || diskConfig(t, c) != before || calls.Load() != 1 {
		t.Fatal("chain change cleared upstream stop")
	}
}

func TestChainTestValidatesWithoutSaving(t *testing.T) {
	c, h := panelControl(t, nil)
	before := diskConfig(t, c)
	input := probeChainRequest{Enabled: true, LocalProxy: "not-a-proxy", ExitProxy: "alice:private-pass@exit.invalid:8080", Protocol: "http"}
	body, _ := json.Marshal(input)
	w := panelPost(h, "probe-chain/test", string(body))
	if w.Code != 400 || diskConfig(t, c) != before || strings.Contains(w.Body.String(), "private-pass") {
		t.Fatal("chain test saved or exposed credentials")
	}
}

func TestChainCandidateAppendsOrReplacesExitBatch(t *testing.T) {
	c := &control{config: settings.Default()}
	c.config.ProbeChain = &settings.ProbeChain{Enabled: true, LocalProxy: "http://127.0.0.1:7897", ExitProxies: []string{"http://one.invalid:8080"}}
	next, err := c.chainCandidate(probeChainRequest{Enabled: true, ExitProxies: "two.invalid:8081", Protocol: "http", Append: true})
	if err != nil || len(next.ProbeChain.ExitProxies) != 2 {
		t.Fatal(next.ProbeChain, err)
	}
	next, err = c.chainCandidate(probeChainRequest{Enabled: true, ExitProxies: "three.invalid:8082", Protocol: "http"})
	if err != nil || len(next.ProbeChain.ExitProxies) != 1 || !strings.Contains(next.ProbeChain.ExitProxies[0], "three.invalid") {
		t.Fatal(next.ProbeChain, err)
	}
}
