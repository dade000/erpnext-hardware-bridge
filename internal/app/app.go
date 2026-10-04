// Package app verbindet Konfiguration, Geräte, WebSocket-API, Oberfläche
// und Kompat-Route zu einer laufenden Bridge.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/httpapi"
	"erpnext-hardware-bridge/internal/logbuf"
	"erpnext-hardware-bridge/internal/netutil"
	"erpnext-hardware-bridge/internal/printer"
	"erpnext-hardware-bridge/internal/scale/pce"
	"erpnext-hardware-bridge/internal/terminal"
	"erpnext-hardware-bridge/internal/update"
	"erpnext-hardware-bridge/internal/version"
	"erpnext-hardware-bridge/internal/ws"
)

// App ist eine laufende Bridge.
type App struct {
	path   string
	exists atomic.Bool
	cfg    atomic.Pointer[config.Config]
	log    *slog.Logger
	level  *slog.LevelVar
	ring   *logbuf.Ring

	bus *device.Bus
	mgr *device.Manager
	ws  *ws.Server
	upd *update.Updater

	// restart beendet den Prozess so, dass der Dienstverwalter ihn neu
	// startet; nil, wenn die Bridge nicht als Dienst läuft.
	restart func()
	stop    chan struct{}

	boundPort int
	loopback  []*http.Server

	mu       sync.Mutex // serialisiert Update
	compat   *http.Server
	compatOn config.HTTPCompat
	warnMu   sync.Mutex
	warnings []string
}

// New baut die Bridge, startet aber noch nichts.
func New(path string, cfg *config.Config, exists bool, log *slog.Logger, level *slog.LevelVar, ring *logbuf.Ring) *App {
	a := &App{path: path, log: log, level: level, ring: ring, bus: device.NewBus(), stop: make(chan struct{})}
	a.upd = update.New(version.Version)
	a.cfg.Store(cfg)
	a.exists.Store(exists)
	a.mgr = device.NewManager(a.bus, log)
	a.mgr.Register("scale", "pce_pb", pce.New)
	printer.Register(a.mgr)
	terminal.Register(a.mgr, func() []string { return a.Config().AllowedOrigins })
	registerOptional(a.mgr) // Kamera usw. je nach Build-Tag
	a.ws = ws.NewServer(a.Config, a.mgr, a.bus, log)
	level.Set(logbuf.ParseLevel(cfg.LogLevel))
	return a
}

// Start bindet die Listener und startet die Geräte.
func (a *App) Start() error {
	c := a.Config()
	ls, warns, err := netutil.ListenLoopback(c.ListenPort)
	if err != nil {
		return fmt.Errorf("WS-API auf Port %d: %w", c.ListenPort, err)
	}
	for _, w := range warns {
		a.warn(w)
	}
	a.boundPort = c.ListenPort
	handler := httpapi.NewLoopback(a, c.ListenPort, a.log)
	for _, l := range ls {
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		a.loopback = append(a.loopback, srv)
		go func(l netutil.Listener) {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Error("Loopback-Server beendet", "family", l.Family, "err", err)
			}
		}(l)
		a.log.Info("WS-API und Oberfläche bereit", "addr", l.Addr().String(), "url", fmt.Sprintf("http://localhost:%d/", c.ListenPort))
	}
	a.mgr.Apply(c.Devices)
	a.applyCompat(c.HTTPCompat)
	if !a.ConfigExists() {
		a.log.Warn("Noch keine Konfiguration – Bridge über die Oberfläche einrichten", "path", a.path)
	}
	go a.updateLoop()
	return nil
}

// updateLoop fragt kurz nach dem Start und danach einmal am Tag nach einem
// neuen Release. Es wird nur gefragt, nie installiert; die Oberfläche zeigt
// den Hinweis. Selbst gebaute Versionen fragen nicht von selbst.
func (a *App) updateLoop() {
	if a.upd.Status().Development {
		return
	}
	wait := time.Minute
	for {
		select {
		case <-a.stop:
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		if a.Config().NoUpdateCheck {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		info, err := a.upd.Check(ctx)
		cancel()
		switch {
		case err != nil:
			a.log.Info("Update-Prüfung nicht möglich", "err", err)
		case info.Available:
			a.log.Info("Neue Version verfügbar", "current", info.Current, "latest", info.Latest)
		}
	}
}

// SetRestart übergibt, wie der Prozess für einen Neustart durch den
// Dienstverwalter beendet wird.
func (a *App) SetRestart(fn func()) { a.restart = fn }

// Shutdown beendet alles geordnet.
func (a *App) Shutdown(ctx context.Context) {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	for _, s := range a.loopback {
		_ = s.Shutdown(ctx)
	}
	a.mu.Lock()
	if a.compat != nil {
		_ = a.compat.Shutdown(ctx)
	}
	a.mu.Unlock()
	a.mgr.Stop()
}

// Update speichert und wendet eine neue Konfiguration an.
func (a *App) Update(c *config.Config) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := config.Save(a.path, c); err != nil {
		return false, fmt.Errorf("Speichern nach %s: %w", a.path, err)
	}
	a.cfg.Store(c)
	a.exists.Store(true)
	a.level.Set(logbuf.ParseLevel(c.LogLevel))
	a.mgr.Apply(c.Devices)
	a.applyCompatLocked(c.HTTPCompat)
	return a.RestartRequired(), nil
}

// RestartRequired: der WS-Port lässt sich nicht im laufenden Betrieb wechseln
// (die Oberfläche selbst hängt daran).
func (a *App) RestartRequired() bool { return a.Config().ListenPort != a.boundPort }

func (a *App) applyCompat(h config.HTTPCompat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyCompatLocked(h)
}

// applyCompatLocked startet den Kompat-Server neu, wenn sich Aktivierung
// oder Adresse geändert haben. Die übrigen Felder liest der Handler bei
// jeder Anfrage aus der aktuellen Konfiguration.
func (a *App) applyCompatLocked(h config.HTTPCompat) {
	if a.compat != nil && h.Enabled && a.compatOn.Listen == h.Listen {
		a.compatOn = h
		return
	}
	if a.compat != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.compat.Shutdown(ctx)
		cancel()
		a.compat = nil
		a.log.Info("HTTP-Kompat gestoppt")
	}
	a.compatOn = h
	a.clearWarn("HTTP-Kompat")
	if !h.Enabled {
		return
	}
	l, err := net.Listen("tcp", h.Listen)
	if err != nil {
		a.warn(fmt.Sprintf("HTTP-Kompat: %s nicht gebunden: %v", h.Listen, err))
		a.log.Error("HTTP-Kompat nicht gestartet", "listen", h.Listen, "err", err)
		return
	}
	srv := &http.Server{
		Handler:           httpapi.NewCompat(func() config.HTTPCompat { return a.Config().HTTPCompat }, a.mgr, a.log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.compat = srv
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("HTTP-Kompat beendet", "err", err)
		}
	}()
	if !netutil.IsLoopbackBind(h.Listen) && h.BasicAuth.User == "" {
		a.log.Warn("HTTP-Kompat lauscht im Netz ohne Basic-Auth", "listen", h.Listen)
	}
	a.log.Info("HTTP-Kompat bereit", "listen", l.Addr().String(), "legacy_upstream", h.LegacyUpstream)
}

func (a *App) warn(w string) {
	a.warnMu.Lock()
	a.warnings = append(a.warnings, w)
	a.warnMu.Unlock()
}

func (a *App) clearWarn(prefix string) {
	a.warnMu.Lock()
	defer a.warnMu.Unlock()
	out := a.warnings[:0]
	for _, w := range a.warnings {
		if len(w) < len(prefix) || w[:len(prefix)] != prefix {
			out = append(out, w)
		}
	}
	a.warnings = out
}

// --- httpapi.Runtime ---

func (a *App) Config() *config.Config   { return a.cfg.Load() }
func (a *App) ConfigPath() string       { return a.path }
func (a *App) ConfigExists() bool       { return a.exists.Load() }
func (a *App) Manager() *device.Manager { return a.mgr }
func (a *App) WS() *ws.Server           { return a.ws }
func (a *App) Logs() []string           { return a.ring.Lines() }
func (a *App) Updater() *update.Updater { return a.upd }

// Restart startet den Dienst neu, wenn die Bridge als Dienst läuft.
func (a *App) Restart() bool {
	if a.restart == nil {
		return false
	}
	a.log.Info("Dienst wird neu gestartet")
	a.restart()
	return true
}

// Status liefert alles, was die Statusseite anzeigt.
func (a *App) Status() map[string]any {
	c := a.Config()
	a.warnMu.Lock()
	warns := append([]string(nil), a.warnings...)
	a.warnMu.Unlock()
	return map[string]any{
		"bridge":           a.ws.Info(),
		"config_path":      a.path,
		"config_exists":    a.ConfigExists(),
		"restart_required": a.RestartRequired(),
		"warnings":         warns,
		"devices":          a.mgr.Statuses(),
		"update":           a.upd.Status(),
		"clients":          a.ws.Clients(),
		"config":           map[string]any{"allowed_origins": c.AllowedOrigins, "http_compat_enabled": c.HTTPCompat.Enabled},
	}
}

var _ httpapi.Runtime = (*App)(nil)
