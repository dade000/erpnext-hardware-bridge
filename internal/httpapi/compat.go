package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/scale/pce"
)

// NewCompat baut die Übergangsroute für den Frappe-Server. /weight liefert
// exakt das Format der Flask-App, damit read_scale_weight() in der Parcel
// Station unverändert weiterläuft. Alles andere geht an legacy_upstream
// (die Flask-App mit der Kamera), solange die Kamera nicht portiert ist.
func NewCompat(cfg func() config.HTTPCompat, mgr *device.Manager, log *slog.Logger) http.Handler {
	var (
		mu       sync.Mutex
		proxyFor string
		proxy    *httputil.ReverseProxy
	)
	upstream := func(target string) *httputil.ReverseProxy {
		mu.Lock()
		defer mu.Unlock()
		if proxy == nil || proxyFor != target {
			u, _ := url.Parse(target) // in Validate geprüft
			proxy = httputil.NewSingleHostReverseProxy(u)
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				log.Warn("Legacy-Upstream nicht erreichbar", "upstream", target, "path", r.URL.Path, "err", err)
				writeJSON(w, http.StatusBadGateway, map[string]string{"status": "error", "message": "legacy upstream: " + err.Error()})
			}
			proxyFor = target
		}
		return proxy
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := cfg()
		if ba := c.BasicAuth; ba.User != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(ba.User)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(ba.Password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="hardware-bridge"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path == "/weight" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			serveWeight(w, r, c, mgr, log)
			return
		}
		if c.LegacyUpstream != "" {
			upstream(c.LegacyUpstream).ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "error", "message": "not available in this bridge build"})
	})
}

func serveWeight(w http.ResponseWriter, r *http.Request, c config.HTTPCompat, mgr *device.Manager, log *slog.Logger) {
	dev, derr := mgr.Lookup("scale", c.ScaleDevice)
	if derr != nil {
		// 500 + status=error ist genau das, was read_scale_weight() als
		// scale_offline übersetzt ("Plug it in and try again").
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": derr.Message})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	res, err := dev.Handle(ctx, "read", nil)
	if err != nil {
		var de *device.Error
		msg := err.Error()
		if errors.As(err, &de) {
			msg = de.Message
		}
		log.Info("Kompat /weight fehlgeschlagen", "err", msg)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": msg})
		return
	}
	rd, _ := res.(pce.Reading)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "success",
		"weight_kg": rd.KG,
		"raw":       rd.Raw,
		"stable":    rd.Stable,
	})
}
