// Package httpapi stellt die HTTP-Seite der Bridge: Oberfläche, JSON-API und
// WebSocket auf Loopback sowie die Kompat-Route für den Frappe-Server.
package httpapi

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/netutil"
	"erpnext-hardware-bridge/internal/serialport"
	"erpnext-hardware-bridge/internal/ws"
)

// Mask ersetzt Geheimnisse in der Oberfläche. Kommt der Wert unverändert
// zurück, bleibt das gespeicherte Geheimnis erhalten.
const Mask = "********"

//go:embed ui/index.html
var uiFS embed.FS

var indexTmpl = template.Must(template.ParseFS(uiFS, "ui/index.html"))

// Runtime ist das, was die Oberfläche von der laufenden Bridge braucht.
type Runtime interface {
	Config() *config.Config
	ConfigPath() string
	ConfigExists() bool
	Update(*config.Config) (restartRequired bool, err error)
	Manager() *device.Manager
	WS() *ws.Server
	Logs() []string
	Status() map[string]any
}

// NewLoopback baut den Handler für 127.0.0.1/::1. port ist der gebundene
// Port (für die Host-Prüfung).
func NewLoopback(rt Runtime, port int, log *slog.Logger) http.Handler {
	csrf := randomToken()
	mux := http.NewServeMux()

	mux.Handle("/ws", rt.WS())

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		_ = indexTmpl.Execute(w, map[string]string{"CSRF": csrf})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		devs := []map[string]any{}
		for _, s := range rt.Manager().Statuses() {
			devs = append(devs, map[string]any{"id": s.ID, "kind": s.Kind, "state": s.State})
		}
		info := rt.WS().Info()
		info["ok"] = true
		info["devices"] = devs
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, rt.Status())
	})

	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, rt.Logs())
	})

	mux.HandleFunc("GET /api/ports", func(w http.ResponseWriter, r *http.Request) {
		ports, err := serialport.List()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, ports)
	})

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		c := rt.Config().Clone()
		if c.Token != "" {
			c.Token = Mask
		}
		if c.HTTPCompat.BasicAuth.Password != "" {
			c.HTTPCompat.BasicAuth.Password = Mask
		}
		writeJSON(w, 200, map[string]any{
			"config":  c,
			"path":    rt.ConfigPath(),
			"exists":  rt.ConfigExists(),
			"drivers": rt.Manager().Drivers(),
		})
	})

	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c config.Config
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []string{"JSON: " + err.Error()}})
			return
		}
		old := rt.Config()
		if c.Token == Mask {
			c.Token = old.Token
		}
		if c.HTTPCompat.BasicAuth.Password == Mask {
			c.HTTPCompat.BasicAuth.Password = old.HTTPCompat.BasicAuth.Password
		}
		c.ApplyDefaults()
		if err := c.Validate(); err != nil {
			writeJSON(w, 400, map[string]any{"errors": splitErrors(err)})
			return
		}
		restart, err := rt.Update(&c)
		if err != nil {
			writeJSON(w, 500, map[string]any{"errors": splitErrors(err)})
			return
		}
		log.Info("Konfiguration über die Oberfläche gespeichert", "path", rt.ConfigPath())
		writeJSON(w, 200, map[string]any{"ok": true, "restart_required": restart})
	})

	mux.HandleFunc("POST /api/devices/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		dev := rt.Manager().Get(r.PathValue("id"))
		if dev == nil {
			writeJSON(w, 404, map[string]any{"ok": false, "error": device.Errf("device_missing", "Gerät nicht gefunden (erst speichern?)")})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		res, err := dev.Handle(ctx, "read", nil)
		if err != nil {
			var de *device.Error
			if !errors.As(err, &de) {
				de = device.Errf("unexpected", err.Error())
			}
			writeJSON(w, 200, map[string]any{"ok": false, "error": de})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": res})
	})

	return guard(mux, port, csrf, log)
}

// guard setzt die Loopback-Regeln durch:
//   - Host-Header muss localhost/127.0.0.1/[::1] mit unserem Port sein
//     (verhindert DNS-Rebinding: fremde Domain, die auf 127.0.0.1 zeigt);
//   - schreibende Anfragen brauchen das CSRF-Token der Seite und dürfen nur
//     von der eigenen Oberfläche kommen (Sec-Fetch-Site / Origin).
//
// /ws prüft den Origin selbst gegen allowed_origins.
func guard(next http.Handler, port int, csrf string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !netutil.IsLoopbackHost(r.Host, port) {
			log.Warn("Anfrage mit fremdem Host-Header abgelehnt", "host", r.Host, "remote", r.RemoteAddr)
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.URL.Path != "/ws" {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				http.Error(w, "cross-site request", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !netutil.IsLoopbackOrigin(o, port) {
				http.Error(w, "cross-origin request", http.StatusForbidden)
				return
			}
			if r.Header.Get("X-Bridge-CSRF") != csrf {
				http.Error(w, "csrf token missing", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func splitErrors(err error) []string {
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
