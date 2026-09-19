package service

import (
	"context"
	"crypto/subtle"
	"embed"
	"net/http"
	"strings"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/gateway"
)

//go:embed web/*
var panelFiles embed.FS

// The browser gets only static assets without a token. All data and actions use
// a bearer token kept in sessionStorage, never cookies or a URL query string.
func controlHandler(host, token string, c *control, tickets ...*browserTicket) http.Handler {
	gatewayHandler := gateway.ProtectLocal(host, c)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !strings.EqualFold(r.Host, host) {
			http.Error(w, "仅接受本机固定地址", 403)
			return
		}
		if r.URL.Path == "/admin" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/_sleep/status" {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
				http.Error(w, "拒绝跨站请求", 403)
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "拒绝跨站请求", 403)
				return
			}
			if r.URL.Path == "/admin/api/launch" && len(tickets) > 0 && tickets[0] != nil {
				tickets[0].exchange(w, r, token)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/admin/api/") || r.URL.Path == "/_sleep/status" {
				if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
					reply(w, 401, map[string]string{"error": "管理口令不正确或服务已重启，请从终端复制新口令。"})
					return
				}
				if r.URL.Path == "/admin/api/new-launch" && len(tickets) > 0 && tickets[0] != nil {
					if r.Method != "POST" {
						w.WriteHeader(405)
						return
					}
					value, err := tickets[0].renew()
					if err != nil {
						reply(w, 500, map[string]string{"error": "无法生成启动凭证"})
						return
					}
					reply(w, 200, map[string]string{"ticket": value})
					return
				}
				if r.URL.Path == "/_sleep/status" {
					if r.Method != "GET" {
						w.WriteHeader(405)
						return
					}
					reply(w, 200, c.status())
					return
				}
				c.api(w, r)
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" {
				w.WriteHeader(405)
				return
			}
			file, mime := "", ""
			switch r.URL.Path {
			case "/admin/":
				file, mime = "index.html", "text/html; charset=utf-8"
			case "/admin/upgrade.js":
				file, mime = "upgrade.js", "text/javascript; charset=utf-8"
			case "/admin/environment_check.js":
				file, mime = "environment_check.js", "text/javascript; charset=utf-8"
			case "/admin/chain.js":
				file, mime = "chain.js", "text/javascript; charset=utf-8"
			case "/admin/app.js":
				file, mime = "app.js", "text/javascript; charset=utf-8"
			case "/admin/style.css":
				file, mime = "style.css", "text/css; charset=utf-8"
			default:
				http.NotFound(w, r)
				return
			}
			data, _ := panelFiles.ReadFile("web/" + file)
			w.Header().Set("Content-Type", mime)
			if r.Method == "GET" {
				_, _ = w.Write(data)
			}
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		gatewayHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}
