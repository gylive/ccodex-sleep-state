package proxyroute

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
)

// All destinations are synthetic. The first hop accepts the second handshake
// on the same stream, so bypassing it would require resolving exit.invalid.
func TestProbeChainTwoHopsAndNormalTraffic(t *testing.T) {
	QuietCore()
	for _, protocol := range []string{"http", "socks5"} {
		t.Run(protocol, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			results := make(chan error, 2)
			go func() {
				for i := 0; i < 2; i++ {
					conn, err := listener.Accept()
					if err != nil {
						results <- err
						return
					}
					func() {
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(3 * time.Second))
						r, err := http.ReadRequest(bufio.NewReader(conn))
						if err != nil {
							results <- err
							return
						}
						wantTarget := "exit.invalid:8080"
						if i == 1 {
							wantTarget = "test.invalid:443"
						}
						wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("local:local-pass"))
						if r.Method != "CONNECT" || r.Host != wantTarget || r.Header.Get("Proxy-Authorization") != wantAuth || r.Header.Get("Authorization") != "" {
							results <- errors.New("first hop destination or credentials differ")
							return
						}
						fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
						if i == 0 {
							if protocol == "http" {
								r, err := http.ReadRequest(bufio.NewReader(conn))
								wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:synthetic-password"))
								if err != nil || r.Method != "CONNECT" || r.Host != "test.invalid:443" || r.Header.Get("Proxy-Authorization") != wantAuth || r.Header.Get("Authorization") != "" {
									results <- errors.New("second hop destination or credentials differ")
									return
								}
								fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
							} else if err := socksHandshake(conn); err != nil {
								results <- err
								return
							}
						}
						b := make([]byte, 4)
						_, err = io.ReadFull(conn, b)
						if err == nil {
							_, err = conn.Write(b)
						}
						results <- err
					}()
				}
			}()
			chain := settings.ProbeChain{Enabled: true, LocalProxy: "http://local:local-pass@" + listener.Addr().String(), ExitProxy: protocol + "://alice:synthetic-password@exit.invalid:8080"}
			route, err := BuildProbeChain(chain)
			if err != nil {
				t.Fatal(err)
			}
			defer route.Close()
			for _, transport := range []*http.Transport{route.ProbeTransport, route.Transport} {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				conn, err := transport.DialContext(ctx, "tcp", "test.invalid:443")
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				conn.Write([]byte("ping"))
				b := make([]byte, 4)
				_, err = io.ReadFull(conn, b)
				conn.Close()
				cancel()
				if err != nil || string(b) != "ping" {
					t.Fatal("tunnel failed", err)
				}
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProbeChainFailureNeverFallsBackAndCancels(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprint(stall), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var attempts atomic.Int32
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				attempts.Add(1)
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				if !stall {
					fmt.Fprint(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
				}
				io.Copy(io.Discard, conn)
			}()
			route, err := BuildProbeChain(settings.ProbeChain{LocalProxy: "http://" + listener.Addr().String(), ExitProxy: "http://alice:secret@exit.invalid:8080"})
			if err != nil {
				t.Fatal(err)
			}
			defer route.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			started := time.Now()
			conn, err := route.ProbeTransport.DialContext(ctx, "tcp", "test.invalid:443")
			if conn != nil {
				conn.Close()
			}
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "exit.invalid") || time.Since(started) > time.Second {
				t.Fatal("chain failure did not stop safely", err)
			}
			wg.Wait()
			if attempts.Load() != 1 {
				t.Fatal("unexpected retry")
			}
		})
	}
}

func TestProbeChainLoadSkipsDormantSources(t *testing.T) {
	c := settings.Default()
	c.Subscriptions = []settings.Source{{File: "missing-dormant-subscription"}}
	c.ProbeChain = &settings.ProbeChain{Enabled: true, LocalProxy: "http://127.0.0.1:7897", ExitProxy: "http://exit.invalid:8080"}
	routes, err := Load(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer routes[0].Close()
	if len(routes) != 1 || routes[0].ProbeTransport == nil {
		t.Fatal("chain was not isolated from pool")
	}
	c.ProbeChain.Enabled = false
	if _, err := Load(context.Background(), c); err == nil {
		t.Fatal("disabled mode did not restore old sources")
	}
}

func TestProbeChainsBuildOneRoutePerExit(t *testing.T) {
	c := settings.ProbeChain{Enabled: true, LocalProxy: "http://127.0.0.1:7897", ExitProxies: []string{
		"http://one.invalid:8080", "socks5://two.invalid:8081",
	}}
	routes, err := BuildProbeChains(c)
	if err != nil || len(routes) != 2 {
		t.Fatal(len(routes), err)
	}
	for i, route := range routes {
		if route.ProbeTransport == nil || route.Transport == nil || route.ID == routes[(i+1)%2].ID {
			t.Fatal("routes are not independent probe exits")
		}
		route.Close()
	}
}
