package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
	"github.com/klauspost/compress/zstd"
)

func encodeRequest(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "gzip":
		var wire bytes.Buffer
		encoder := gzip.NewWriter(&wire)
		if _, err := encoder.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		return wire.Bytes()
	case "zstd":
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Close()
		return encoder.EncodeAll(body, nil)
	default:
		return body
	}
}

func TestCompressedGenerationForwardsDecodedBody(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if string(body) != generation {
					t.Error("decoded body changed")
				}
				if r.Header.Get("Content-Encoding") != "" || r.ContentLength != int64(len(generation)) {
					t.Error("compressed framing was forwarded")
				}
				complete(w, fakeToken(10, 42))
			}))
			e.SetInjection(false)
			wire := encodeRequest(t, encoding, []byte(generation))
			r := request(string(wire), "compressed-request-token")
			r.Header.Set("Content-Encoding", encoding)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
		})
	}
}

func TestCompressedV2CompactionIsRecognized(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			body := `{"model":"gpt-6-astra","input":[{"type":"compaction_trigger"}],"stream":true}`
			var calls atomic.Int32
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				if string(data) != body || r.Header.Get(turnstate.Header) != "client-state" {
					t.Error("compressed compaction was modified or probed")
				}
				complete(w, fakeToken(13, 43))
			}))
			wire := encodeRequest(t, encoding, []byte(body))
			r := request(string(wire), "compressed-compaction-token")
			r.Header.Set("Content-Encoding", encoding)
			r.Header.Set(turnstate.Header, "client-state")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestBadCompressedRequestsNeverReachUpstream(t *testing.T) {
	goodGzip := encodeRequest(t, "gzip", []byte(generation))
	cases := []struct {
		name, encoding string
		body           []byte
		status         int
	}{
		{"bad gzip", "gzip", []byte("broken"), 400},
		{"truncated gzip", "gzip", goodGzip[:len(goodGzip)-4], 400},
		{"bad zstd", "zstd", []byte("broken"), 400},
		{"unsupported", "br", []byte("broken"), 415},
		{"stacked encodings", "gzip, zstd", goodGzip, 415},
		{"oversized wire", "gzip", bytes.Repeat([]byte{'a'}, maxRequestBytes+1), 413},
		{"oversized zstd window", "zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x78, 1, 0, 0}, 413},
	}
	for _, encoding := range []string{"gzip", "zstd"} {
		cases = append(cases, struct {
			name, encoding string
			body           []byte
			status         int
		}{"expanded " + encoding, encoding, encodeRequest(t, encoding, bytes.Repeat([]byte{'a'}, maxRequestBytes+1)), 413})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid encoded request reached upstream") }))
			e.config.RequestLimitMiB = 16
			e.config.ZstdWindowMiB = 16
			r := request(string(tt.body), "bad-encoding-token")
			r.Header.Set("Content-Encoding", tt.encoding)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.status, w.Body.String())
			}
		})
	}
}

func TestCompressedJSONStillValidatesModel(t *testing.T) {
	e, _ := testEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid model reached upstream") }))
	wire := encodeRequest(t, "zstd", []byte(strings.ReplaceAll(generation, "gpt-6-astra", "")))
	r := request(string(wire), "bad-model-token")
	r.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_model") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
