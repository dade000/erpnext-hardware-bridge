// Package camera ist der Kameratreiber der Bridge (Photo Station).
//
// Der Treiber hält eine Sitzung zur Kamera dauerhaft offen, liefert Fotos
// (camera.capture) und, solange jemand zusieht, das Live-Bild der Kamera
// (camera.frame). Die eigentliche Ansteuerung steckt hinter Backend: libgphoto2
// unter Linux (Paket gphoto) oder ein Testbild (Paket simulator).
//
// Alle Aufrufe ans Backend laufen in einer einzigen Goroutine, die an ihren
// Betriebssystem-Thread gebunden ist: libgphoto2 ist nicht threadsicher.
package camera

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// Kind ist die Geräteklasse.
const Kind = "camera"

const (
	defaultFPS      = 15
	maxFPS          = 30
	defaultHold     = 30 * time.Second // Live-Bild der Kamera so lange ohne Zuhörer weiterlaufen lassen
	keepalive       = 30 * time.Second
	maxBackoff      = 5 * time.Second
	requestDeadline = 30 * time.Second
	// maxPreviewFails: so viele Live-Bilder dürfen hintereinander fehlen,
	// bevor die Sitzung neu aufgebaut wird. Canon liefert direkt nach dem
	// Einschalten des Live-Bilds oder nach einer Aufnahme kurz »busy«.
	maxPreviewFails = 15
)

// Info beschreibt die verbundene Kamera.
type Info struct {
	Model string `json:"model"`
	Port  string `json:"port,omitempty"`
	// Library: Version der Ansteuerung (libgphoto2), für /health.
	Library string `json:"library,omitempty"`
}

// Backend steuert eine Kamera an. Alle Methoden werden aus derselben
// Goroutine aufgerufen.
type Backend interface {
	Open() (Info, error)
	Close()
	// Capture löst aus und liefert das JPEG.
	Capture() ([]byte, error)
	// Preview liefert ein Bild des Live-Bilds (schaltet es bei Bedarf ein).
	Preview() ([]byte, error)
	// Config liest eine Einstellung samt Auswahlmöglichkeiten.
	Config(name string) (value string, choices []string, err error)
	// SetConfig ändert eine Einstellung. Bei Auswahlfeldern zählt der Text
	// ohne Rücksicht auf Groß-/Kleinschreibung.
	SetConfig(name, value string) error
}

// Error ist ein Fehler des Backends. Lost heißt: die Sitzung ist weg
// (Kamera aus, Kabel ab, belegt) und muss neu aufgebaut werden.
type Error struct {
	Code    string
	Message string
	Lost    bool
}

func (e *Error) Error() string { return e.Message }

// Opener baut ein frisches Backend.
type Opener func(cfg config.DeviceConfig) Backend

// Stats zählt für die Statusseite.
type Stats struct {
	Captures  int    `json:"captures"`
	Frames    int    `json:"frames"`
	Failures  int    `json:"failures"`
	Reopens   int    `json:"reopens"`
	LastError string `json:"last_error,omitempty"`
	LastCapMS int    `json:"last_capture_ms,omitempty"`
	FPS       int    `json:"fps,omitempty"`
}

// Last ist der letzte bekannte Zustand für Status und Desk.
type Last struct {
	Model       string `json:"model,omitempty"`
	Port        string `json:"port,omitempty"`
	Battery     string `json:"battery,omitempty"`
	ImageFormat string `json:"image_format,omitempty"`
	Live        bool   `json:"live"`
	TS          string `json:"ts,omitempty"`
}

type request struct {
	ctx    context.Context
	cmd    string
	params json.RawMessage
	done   chan result
}

type result struct {
	v   any
	err error
}

// Camera ist der laufende Treiber.
type Camera struct {
	cfg  config.DeviceConfig
	bus  *device.Bus
	log  *slog.Logger
	st   *device.StatusHolder
	open Opener

	reqs   chan request
	wake   chan struct{}
	demand atomic.Int32

	statsMu sync.Mutex
	stats   Stats
	last    Last
	info    Info

	// nur in der Run-Goroutine
	be        Backend
	live      bool // Live-Bild der Kamera ist an
	seq       int
	fails     int
	idleSince time.Time
}

// New baut den Treiber mit dem übergebenen Backend.
func New(open Opener) device.Factory {
	return func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWith(cfg, bus, log, open)
	}
}

// NewWith ist New ohne Factory-Umweg (Tests).
func NewWith(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, open Opener) *Camera {
	port := cfg.Port.String()
	if cfg.Port.Path == "" && cfg.Port.Match == nil {
		port = "automatisch"
	}
	return &Camera{
		cfg:  cfg,
		bus:  bus,
		log:  log,
		st:   device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, port),
		open: open,
		reqs: make(chan request),
		wake: make(chan struct{}, 1),
	}
}

func (c *Camera) ID() string   { return c.cfg.ID }
func (c *Camera) Kind() string { return Kind }

func (c *Camera) Status() device.Status {
	st := c.st.Get()
	c.statsMu.Lock()
	st.Stats = c.stats
	l := c.last
	st.Last = l
	c.statsMu.Unlock()
	return st
}

// Info liefert die zuletzt verbundene Kamera (für /health).
func (c *Camera) Info() Info {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.info
}

func (c *Camera) bump(fn func(*Stats, *Last)) {
	c.statsMu.Lock()
	fn(&c.stats, &c.last)
	c.statsMu.Unlock()
}

// AddDemand zählt Zuschauer des Live-Bilds.
func (c *Camera) AddDemand(delta int) {
	c.demand.Add(int32(delta))
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Camera) fps() int {
	f := c.cfg.LiveFPS
	if f <= 0 {
		f = defaultFPS
	}
	return min(f, maxFPS)
}

func (c *Camera) hold() time.Duration {
	if c.cfg.LiveHoldS > 0 {
		return time.Duration(c.cfg.LiveHoldS) * time.Second
	}
	return defaultHold
}

// Handle: capture, preview, focus, reset, test.
func (c *Camera) Handle(ctx context.Context, cmd string, params json.RawMessage) (any, error) {
	switch cmd {
	case "capture", "preview", "focus", "reset", "test", "read":
	default:
		return nil, device.Errf("unknown_method", "camera."+cmd+" gibt es nicht")
	}
	if cmd == "read" {
		cmd = "test"
	}
	ctx, cancel := context.WithTimeout(ctx, requestDeadline)
	defer cancel()
	r := request{ctx: ctx, cmd: cmd, params: params, done: make(chan result, 1)}
	select {
	case c.reqs <- r:
	case <-ctx.Done():
		return nil, device.Errf("busy", "Kamera ist beschäftigt")
	}
	select {
	case res := <-r.done:
		return res.v, res.err
	case <-ctx.Done():
		return nil, device.Errf("timeout", "Kamera hat nicht rechtzeitig geantwortet")
	}
}

// Run hält die Sitzung offen, bis ctx endet.
func (c *Camera) Run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer c.disconnect()

	backoff := 500 * time.Millisecond
	var pending *request
	for ctx.Err() == nil {
		if c.be == nil {
			if err := c.connect(); err != nil {
				if pending != nil {
					pending.done <- result{err: offlineErr(err)}
					pending = nil
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
				case r := <-c.reqs:
					// Eine Anfrage löst sofort einen neuen Versuch aus:
					// Kamera eingeschaltet, Knopf gedrückt – geht.
					pending = &r
				}
				continue
			}
			backoff = 500 * time.Millisecond
		}
		c.serve(ctx, pending)
		pending = nil
		c.disconnect()
	}
}

func offlineErr(err error) error {
	var ce *Error
	if errors.As(err, &ce) {
		return device.Errf("camera_offline", ce.Message)
	}
	return device.Errf("camera_offline", err.Error())
}

func (c *Camera) connect() error {
	be := c.open(c.cfg)
	info, err := be.Open()
	if err != nil {
		be.Close()
		msg := err.Error()
		c.st.Set(device.StateOffline, msg)
		c.bump(func(s *Stats, _ *Last) { s.LastError = msg })
		return err
	}
	c.be = be
	c.live = false
	c.fails = 0
	c.idleSince = time.Now()

	// Fotos landen im RAM der Kamera, nicht auf der Karte: nichts läuft voll,
	// nichts bleibt liegen.
	if err := be.SetConfig("capturetarget", "Internal RAM"); err != nil {
		c.log.Debug("capturetarget nicht gesetzt", "err", err)
	}
	if f := strings.TrimSpace(c.cfg.ImageFormat); f != "" {
		if err := be.SetConfig("imageformat", f); err != nil {
			c.log.Warn("Bildqualität nicht gesetzt", "wanted", f, "err", err)
		}
	}
	fmtNow, _, _ := be.Config("imageformat")
	battery, _, _ := be.Config("batterylevel")

	c.statsMu.Lock()
	c.info = info
	c.last = Last{Model: info.Model, Port: info.Port, Battery: battery, ImageFormat: fmtNow, TS: now()}
	c.statsMu.Unlock()
	if info.Port != "" {
		c.st.Update(func(s *device.Status) { s.Port = info.Port })
	}
	msg := info.Model
	if battery != "" {
		msg += " · Akku " + battery
	}
	c.st.Set(device.StateOnline, msg)
	c.log.Info("Kamera verbunden", "model", info.Model, "port", info.Port, "imageformat", fmtNow)
	return nil
}

func (c *Camera) disconnect() {
	if c.be == nil {
		return
	}
	if c.live {
		_ = c.be.SetConfig("viewfinder", "0")
	}
	c.be.Close()
	c.be = nil
	c.live = false
	c.bump(func(_ *Stats, l *Last) { l.Live = false })
}

// serve bearbeitet Anfragen und das Live-Bild, bis die Sitzung verloren
// geht oder ctx endet.
func (c *Camera) serve(ctx context.Context, first *request) {
	if first != nil {
		if c.handle(*first) {
			c.lost()
			return
		}
	}
	ka := time.NewTicker(keepalive)
	defer ka.Stop()
	interval := time.Second / time.Duration(c.fps())
	next := time.Now()
	fpsCount, fpsSince := 0, time.Now()

	for {
		var tick <-chan time.Time
		watching := c.demand.Load() > 0
		switch {
		case watching:
			tick = time.After(time.Until(next))
		case c.live:
			// Niemand sieht zu: Live-Bild der Kamera nach der Haltezeit
			// abschalten (Spiegel runter, Akku und Sensor schonen).
			tick = time.After(time.Until(c.idleSince.Add(c.hold())))
		}

		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			if c.demand.Load() <= 0 {
				c.idleSince = time.Now()
			}
		case r := <-c.reqs:
			if c.handle(r) {
				c.lost()
				return
			}
			next = time.Now()
		case <-ka.C:
			if c.live && watching {
				continue // das Live-Bild hält die Sitzung ohnehin wach
			}
			bat, _, err := c.be.Config("batterylevel")
			if isLost(err) {
				c.fail(err)
				c.lost()
				return
			}
			if bat != "" {
				c.bump(func(_ *Stats, l *Last) { l.Battery = bat })
				c.st.Set(device.StateOnline, c.info.Model+" · Akku "+bat)
			}
		case <-tick:
			if !watching {
				if c.live && c.demand.Load() <= 0 {
					_ = c.be.SetConfig("viewfinder", "0")
					c.live = false
					c.bump(func(_ *Stats, l *Last) { l.Live = false })
					c.log.Debug("Live-Bild der Kamera aus")
				}
				continue
			}
			start := time.Now()
			next = start.Add(interval)
			img, err := c.be.Preview()
			if err != nil {
				c.fails++
				if isLost(err) || c.fails >= maxPreviewFails {
					c.fail(err)
					c.lost()
					return
				}
				next = time.Now().Add(100 * time.Millisecond)
				continue
			}
			c.fails = 0
			if !c.live {
				c.live = true
				c.bump(func(_ *Stats, l *Last) { l.Live = true })
			}
			c.seq++
			c.bus.Publish(device.Event{Name: "camera.frame", Device: c.cfg.ID, Data: &device.Binary{
				Meta: map[string]any{"seq": c.seq, "mime": "image/jpeg", "bytes": len(img), "ts": now()},
				Data: img,
			}})
			fpsCount++
			if el := time.Since(fpsSince); el >= 2*time.Second {
				f := int(float64(fpsCount)/el.Seconds() + 0.5)
				c.bump(func(s *Stats, _ *Last) { s.Frames += fpsCount; s.FPS = f })
				fpsCount, fpsSince = 0, time.Now()
			}
			c.idleSince = time.Now()
		}
	}
}

func (c *Camera) lost() {
	c.bump(func(s *Stats, _ *Last) { s.Reopens++ })
	c.st.Set(device.StateOffline, "Verbindung zur Kamera verloren, verbinde neu…")
	c.log.Warn("Kamera-Sitzung verloren, baue neu auf")
}

func (c *Camera) fail(err error) {
	c.bump(func(s *Stats, _ *Last) { s.Failures++; s.LastError = err.Error() })
}

func isLost(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Lost
}

// handle führt eine Anfrage aus. true heißt: Sitzung verloren.
func (c *Camera) handle(r request) bool {
	if r.ctx.Err() != nil {
		return false // Aufrufer wartet nicht mehr
	}
	var (
		v   any
		err error
	)
	switch r.cmd {
	case "capture":
		v, err = c.capture()
	case "preview":
		var img []byte
		img, err = c.be.Preview()
		if err == nil {
			c.live = true
			c.idleSince = time.Now()
			v = &device.Binary{Meta: map[string]any{"mime": "image/jpeg", "bytes": len(img)}, Data: img}
		}
	case "focus":
		err = c.be.SetConfig("autofocusdrive", "1")
		if err == nil {
			v = map[string]any{"ok": true}
		}
	case "reset":
		r.done <- result{v: map[string]any{"reset": true}}
		return true
	case "test":
		v, err = c.test()
	}
	if err != nil {
		c.fail(err)
		r.done <- result{err: clientErr(err)}
		return isLost(err)
	}
	r.done <- result{v: v}
	return false
}

func clientErr(err error) error {
	var ce *Error
	if errors.As(err, &ce) {
		code := ce.Code
		if code == "" {
			code = "camera_error"
		}
		return device.Errf(code, ce.Message)
	}
	return device.Errf("camera_error", err.Error())
}

func (c *Camera) capture() (any, error) {
	start := time.Now()
	img, err := c.be.Capture()
	if err != nil {
		return nil, err
	}
	ms := int(time.Since(start).Milliseconds())
	meta := map[string]any{"mime": "image/jpeg", "bytes": len(img), "ms": ms, "ts": now()}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(img)); err == nil {
		meta["width"], meta["height"] = cfg.Width, cfg.Height
	}
	c.bump(func(s *Stats, l *Last) { s.Captures++; s.LastCapMS = ms; l.TS = now() })
	c.log.Info("Foto aufgenommen", "bytes", len(img), "ms", ms)
	// Nach der Aufnahme läuft das Live-Bild (falls an) einfach weiter.
	c.idleSince = time.Now()
	return &device.Binary{Meta: meta, Data: img}, nil
}

// test: was die Oberfläche der Bridge unter »Testen« zeigt – Kamera, Akku,
// Bildqualität mit Auswahl und ein Live-Bild.
func (c *Camera) test() (any, error) {
	out := map[string]any{"model": c.info.Model, "port": c.info.Port, "library": c.info.Library}
	if v, ch, err := c.be.Config("imageformat"); err == nil {
		out["image_format"] = map[string]any{"value": v, "choices": ch}
	}
	if v, _, err := c.be.Config("batterylevel"); err == nil {
		out["battery"] = v
	}
	if v, _, err := c.be.Config("capturetarget"); err == nil {
		out["capture_target"] = v
	}
	img, err := c.be.Preview()
	if err != nil {
		if isLost(err) {
			return nil, err
		}
		out["preview_error"] = err.Error()
	} else {
		c.live = true
		c.idleSince = time.Now()
		out["preview"] = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img)
	}
	return out, nil
}

func now() string { return time.Now().Format(time.RFC3339Nano) }

// Errorf baut einen Backend-Fehler.
func Errorf(lost bool, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Lost: lost}
}
