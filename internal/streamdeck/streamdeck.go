// Package streamdeck bindet ein Elgato Stream Deck (Original V2, MK.2) als
// Tastenfeld an. Die Belegung steht fest in bridge.yaml: Beschriftung, Farbe
// und Aktion je Taste. Ein Druck geht als Event streamdeck.key mit der Aktion
// an alle Seiten, die das Gerät abonniert haben; was die Aktion bewirkt,
// entscheidet die Seite (Photo Station: photo.shoot …).
//
// Hört keine Seite zu, sind die Tasten ausgegraut. So sieht man am Gerät, ob
// ein Druck irgendwo ankommt.
package streamdeck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

const (
	Kind              = "streamdeck"
	defaultBrightness = 70
	maxBackoff        = 10 * time.Second
	pressFlash        = 150 * time.Millisecond
	identifyFor       = 3 * time.Second
)

// ErrNotFound: kein (passendes) Stream Deck angeschlossen.
var ErrNotFound = errors.New("kein Stream Deck angeschlossen")

// Conn ist die HID-Verbindung zu einem Stream Deck.
type Conn interface {
	// Read liefert einen Input-Report (Tastenzustände), blockiert bis dahin.
	Read(p []byte) (int, error)
	Write(p []byte) error
	SetFeature(p []byte) error
	GetFeature(p []byte) (int, error)
	Close() error
}

// Info beschreibt ein gefundenes Gerät.
type Info struct {
	Path   string
	PID    uint16
	Serial string
}

// Opener öffnet das Gerät; serial leer = das erste gefundene.
type Opener func(serial string) (Conn, Info, error)

// Last ist der Stand für Statusseite und »Testen«.
type Last struct {
	Model    string     `json:"model,omitempty"`
	Serial   string     `json:"serial,omitempty"`
	Firmware string     `json:"firmware,omitempty"`
	Key      *int       `json:"key,omitempty"`
	Action   string     `json:"action,omitempty"`
	TS       *time.Time `json:"ts,omitempty"`
}

// Stats zählt Tastendrücke und Neuverbindungen.
type Stats struct {
	Presses int `json:"presses"`
	Reopens int `json:"reopens"`
}

type request struct {
	cmd  string
	key  int
	resp chan result
}

type result struct {
	v   any
	err error
}

// Deck ist der Treiber.
type Deck struct {
	cfg  config.DeviceConfig
	bus  *device.Bus
	log  *slog.Logger
	st   *device.StatusHolder
	open Opener
	keys map[int]config.DeckKey

	demand atomic.Int32
	redraw chan struct{}
	reqs   chan request

	mu    sync.Mutex
	last  Last
	stats Stats
}

// New baut den Treiber für ein echtes Gerät (hidraw, nur Linux).
func New(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	return NewWithOpener(cfg, bus, log, OpenHID)
}

// NewSimulator baut ein Stream Deck ohne Hardware: Tasten lassen sich nur
// über die Oberfläche (oder streamdeck.press) drücken.
func NewSimulator(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	return NewWithOpener(cfg, bus, log, openSimulator)
}

// NewWithOpener erlaubt Tests eine Attrappe statt des echten Geräts.
func NewWithOpener(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, open Opener) *Deck {
	keys := map[int]config.DeckKey{}
	for _, k := range cfg.Keys {
		keys[k.Key] = k
	}
	port := "usb"
	if cfg.Serial != "" {
		port = "usb #" + cfg.Serial
	}
	return &Deck{
		cfg: cfg, bus: bus, log: log, open: open, keys: keys,
		st:     device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, port),
		redraw: make(chan struct{}, 1),
		reqs:   make(chan request),
	}
}

func (d *Deck) ID() string   { return d.cfg.ID }
func (d *Deck) Kind() string { return Kind }

func (d *Deck) Status() device.Status {
	st := d.st.Get()
	d.mu.Lock()
	st.Last, st.Stats = d.last, d.stats
	d.mu.Unlock()
	return st
}

// AddDemand zählt Seiten, die zuhören. Wechselt es zwischen »niemand« und
// »jemand«, werden die Tasten neu gezeichnet (grau bzw. farbig).
func (d *Deck) AddDemand(delta int) {
	n := d.demand.Add(int32(delta))
	if (n > 0) != (n-int32(delta) > 0) {
		select {
		case d.redraw <- struct{}{}:
		default:
		}
	}
}

// Handle: read/test zeigt kurz die Tastennummern am Gerät und liefert die
// Tastenbilder für die Oberfläche; press löst einen Tastendruck aus, als
// wäre die Taste gedrückt worden.
func (d *Deck) Handle(ctx context.Context, cmd string, params json.RawMessage) (any, error) {
	var p struct {
		Key *int `json:"key"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, device.Errf("bad_request", err.Error())
		}
	}
	switch cmd {
	case "read", "test":
		cmd = "test"
	case "press":
		if p.Key == nil || *p.Key < 0 || *p.Key >= config.DeckKeys {
			return nil, device.Errf("bad_request", "key fehlt oder liegt außerhalb 0–14")
		}
	default:
		return nil, device.Errf("unknown_method", "streamdeck."+cmd+" gibt es nicht")
	}
	q := request{cmd: cmd, resp: make(chan result, 1)}
	if p.Key != nil {
		q.key = *p.Key
	}
	select {
	case d.reqs <- q:
	case <-ctx.Done():
		return nil, device.Errf("timeout", "Stream Deck antwortet nicht")
	}
	select {
	case r := <-q.resp:
		return r.v, r.err
	case <-ctx.Done():
		return nil, device.Errf("timeout", "Stream Deck antwortet nicht")
	}
}

// Run hält das Gerät verbunden und verbindet nach Abziehen neu.
func (d *Deck) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, info, err := d.open(d.cfg.Serial)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, ErrNotFound) && d.cfg.Serial != "" && msg == ErrNotFound.Error() {
				msg = "Stream Deck " + d.cfg.Serial + " nicht angeschlossen"
			}
			d.st.Set(device.StateOffline, msg)
			d.waitOffline(ctx, backoff, msg)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		err = d.session(ctx, conn, info)
		if ctx.Err() != nil {
			// Bridge hält an oder Konfiguration geändert: Logo statt
			// veralteter Tasten.
			_ = conn.SetFeature(resetReport())
			_ = conn.Close()
			return
		}
		_ = conn.Close()
		d.mu.Lock()
		d.stats.Reopens++
		d.mu.Unlock()
		d.log.Warn("Stream Deck getrennt", "err", err)
		d.st.Set(device.StateOffline, "Verbindung verloren: "+err.Error())
		d.waitOffline(ctx, backoff, "Stream Deck nicht verbunden")
	}
}

func (d *Deck) waitOffline(ctx context.Context, wait time.Duration, msg string) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case q := <-d.reqs:
			q.resp <- result{err: device.Errf("streamdeck_offline", msg)}
		}
	}
}

type deckConn struct {
	conn  Conn
	model Model
}

func (c deckConn) draw(key int, img []byte) error {
	for _, r := range imageReports(key, img) {
		if err := c.conn.Write(r); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deck) session(ctx context.Context, conn Conn, info Info) error {
	model, ok := ModelByPID(info.PID)
	if !ok {
		model = Models[1]
	}
	c := deckConn{conn: conn, model: model}
	bright := d.cfg.Brightness
	if bright == 0 {
		bright = defaultBrightness
	}
	if err := conn.SetFeature(brightnessReport(bright)); err != nil {
		return err
	}
	last := Last{Model: model.Name, Serial: info.Serial, Firmware: getString(conn, 0x05, 6)}
	if last.Serial == "" {
		last.Serial = getString(conn, 0x06, 2)
	}
	d.mu.Lock()
	d.last = last
	d.mu.Unlock()
	d.st.Update(func(st *device.Status) { st.Port = info.Path })

	if err := d.drawAll(c, -1); err != nil {
		return err
	}
	d.st.Set(device.StateOnline, "")
	d.log.Info("Stream Deck verbunden", "model", model.Name, "serial", last.Serial, "path", info.Path, "firmware", last.Firmware)

	reports := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		for {
			buf := make([]byte, inputReportLen)
			n, err := conn.Read(buf)
			if err != nil {
				errc <- err
				return
			}
			select {
			case reports <- buf[:n]:
			case <-ctx.Done():
				return
			}
		}
	}()
	// Das hängende Read endet mit dem Close in Run.

	prev := make([]bool, model.Keys)
	restore := make(chan int, model.Keys)
	var identify <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case r := <-reports:
			states, err := parseKeys(r, model.Keys)
			if err != nil {
				continue
			}
			for i, down := range states {
				if down == prev[i] {
					continue
				}
				prev[i] = down
				if down {
					d.pressed(i)
				}
				if identify == nil {
					l := d.look()
					if down {
						l = lookPressed
					}
					if err := c.draw(i, d.keyImage(i, model.Size, l)); err != nil {
						return err
					}
				}
			}
		case i := <-restore:
			if identify == nil && !prev[i] {
				if err := c.draw(i, d.keyImage(i, model.Size, d.look())); err != nil {
					return err
				}
			}
		case <-d.redraw:
			if identify == nil {
				if err := d.drawAll(c, -1); err != nil {
					return err
				}
			}
		case <-identify:
			identify = nil
			if err := d.drawAll(c, -1); err != nil {
				return err
			}
		case q := <-d.reqs:
			switch q.cmd {
			case "press":
				d.pressed(q.key)
				if identify == nil {
					if err := c.draw(q.key, d.keyImage(q.key, model.Size, lookPressed)); err != nil {
						q.resp <- result{err: device.Errf("streamdeck_offline", err.Error())}
						return err
					}
					key := q.key
					time.AfterFunc(pressFlash, func() {
						select {
						case restore <- key:
						default:
						}
					})
				}
				k := d.keys[q.key]
				q.resp <- result{v: map[string]any{"key": q.key, "action": k.Action, "label": k.Label, "listeners": d.demand.Load()}}
			case "test":
				identify = time.After(identifyFor)
				if err := d.drawAll(c, -2); err != nil {
					q.resp <- result{err: device.Errf("streamdeck_offline", err.Error())}
					return err
				}
				q.resp <- result{v: d.testResult(model)}
			}
		}
	}
}

// getString liest Firmware (Report 0x05) oder Seriennummer (0x06).
func getString(conn Conn, report byte, from int) string {
	b := make([]byte, featureLen)
	b[0] = report
	n, err := conn.GetFeature(b)
	if err != nil {
		return ""
	}
	return featureString(b[:n], from)
}

// look: farbig, wenn eine Seite zuhört, sonst grau.
func (d *Deck) look() look {
	if d.demand.Load() > 0 {
		return lookNormal
	}
	return lookIdle
}

// drawAll zeichnet alle Tasten; mode -2 = Tastennummern (»Testen«).
func (d *Deck) drawAll(c deckConn, mode int) error {
	l := d.look()
	if mode == -2 {
		l = lookIdentify
	}
	for i := 0; i < c.model.Keys; i++ {
		if err := c.draw(i, d.keyImage(i, c.model.Size, l)); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deck) keyImage(i, size int, l look) []byte {
	var k *config.DeckKey
	if v, ok := d.keys[i]; ok {
		k = &v
	}
	return encodeJPEG(flip(renderKey(k, i, size, l)))
}

// pressed meldet einen Tastendruck. Tasten ohne Aktion sind nur Schilder.
func (d *Deck) pressed(i int) {
	k, ok := d.keys[i]
	now := time.Now()
	d.mu.Lock()
	d.stats.Presses++
	key := i
	d.last.Key, d.last.Action, d.last.TS = &key, k.Action, &now
	d.mu.Unlock()
	if !ok || k.Action == "" {
		d.log.Debug("Stream-Deck-Taste ohne Aktion", "key", i+1)
		return
	}
	d.log.Debug("Stream-Deck-Taste", "key", i+1, "action", k.Action, "listeners", d.demand.Load())
	d.bus.Publish(device.Event{Name: "streamdeck.key", Device: d.cfg.ID, Data: map[string]any{
		"key": i, "action": k.Action, "label": k.Label, "ts": now.UTC().Format(time.RFC3339Nano),
	}})
}

func (d *Deck) testResult(model Model) map[string]any {
	d.mu.Lock()
	last := d.last
	d.mu.Unlock()
	keys := make([]map[string]any, model.Keys)
	for i := range keys {
		k := d.keys[i]
		var kp *config.DeckKey
		if v, ok := d.keys[i]; ok {
			kp = &v
		}
		img := encodePNG(renderKey(kp, i, model.Size, lookNormal))
		keys[i] = map[string]any{
			"key": i, "label": k.Label, "action": k.Action,
			"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(img),
		}
	}
	return map[string]any{
		"model": last.Model, "serial": last.Serial, "firmware": last.Firmware,
		"port": d.st.Get().Port, "columns": model.Columns, "listeners": d.demand.Load(), "keys": keys,
	}
}
