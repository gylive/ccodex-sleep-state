package settings

import (
	"net/url"
	"strings"
	"testing"
)

func TestChainAddressNormalizationAndValidation(t *testing.T) {
	for _, protocol := range []string{"http", "https", "socks5"} {
		raw, err := NormalizeProxyEndpoint("alice:p@ss:#%word@exit.invalid:8080", protocol)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		password, _ := u.User.Password()
		if u.Scheme != protocol || u.Host != "exit.invalid:8080" || u.User.Username() != "alice" || password != "p@ss:#%word" {
			t.Fatal("shorthand credential parsing changed")
		}
		c := Default()
		c.ProbeChain = &ProbeChain{Enabled: true, LocalProxy: "socks5://127.0.0.1:7897", ExitProxy: raw}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`alice:secret\@exit.invalid:8080`, "http://alice:secret@exit.invalid", "http://alice:secret@exit.invalid:8080/path", "http://alice:secret@exit.invalid:8080?x=1", "http://alice:secret@exit.invalid:8080#x", "ss://alice:secret@exit.invalid:8080"} {
		_, err := NormalizeProxyEndpoint(raw, "http")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("bad endpoint accepted or credentials exposed")
		}
	}
	for _, local := range []string{"http://public.invalid:7897", "http://192.168.1.1:7897", "invalid", "http://127.0.0.1:0"} {
		c := ProbeChain{LocalProxy: local, ExitProxy: "http://alice:secret@exit.invalid:8080"}
		if c.Validate() == nil {
			t.Fatal("nonlocal or invalid first hop allowed")
		}
	}
}

func TestChainRejectsPoolPolicies(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.PoolEnabled = true },
		func(c *Config) { c.EgressMode = "random" },
		func(c *Config) { c.EgressMode, c.EgressRoute = "fixed", "node" },
		func(c *Config) { c.PinnedRoute = "node" },
	} {
		c := Default()
		c.ProbeChain = &ProbeChain{Enabled: true, LocalProxy: "http://127.0.0.1:7897", ExitProxy: "http://exit.invalid:8080"}
		change(&c)
		if c.Validate() == nil {
			t.Fatal("chain can consume or rotate normal egress")
		}
	}
}

func TestMultipleProbeExitsParseDeduplicateAndPreserveOrder(t *testing.T) {
	exits, err := ParseProbeExits("# first\nalice:one@Exit-A.invalid:8080\nalice:one@exit-a.invalid:8080\nbob:two@exit-b.invalid:8081\n", "http")
	if err != nil || len(exits) != 2 || !strings.Contains(exits[0], "exit-a.invalid") || !strings.Contains(exits[1], "exit-b.invalid") {
		t.Fatal(exits, err)
	}
	c := Default()
	c.ProbeChain = &ProbeChain{Enabled: true, LocalProxy: "http://127.0.0.1:7897", ExitProxies: exits}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ProbeChain.ExitURLs(); err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	if _, err := ParseProbeExits("alice:one@exit-a.invalid:8080\nbad", "http"); err == nil {
		t.Fatal("invalid line accepted")
	}
}
