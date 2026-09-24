// Package pce ist der Treiber für PCE-PB-N-Plattformwaagen (z.B. PCE-PB 60N)
// über RS-232 bzw. USB-Seriell.
//
// Protokoll laut Handbuch: "Sx\r\n" fordert die aktuelle Anzeige an,
// "ST\r\n" tariert. Weitere Befehle (Nullstellen, Stabil-Abfrage) sind nicht
// bestätigt und deshalb nicht eingebaut.
package pce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/serialport"
)

const (
	readTimeout     = 2 * time.Second // wie in der Flask-App
	idleInterval    = 5 * time.Second // Lebenszeichen ohne Zuhörer
	maxTimeouts     = 3               // danach Port neu öffnen
	maxBackoff      = 5 * time.Second // Einstecken soll schnell auffallen
	requestDeadline = 4 * time.Second
)

// Reading ist eine Messung.
type Reading struct {
	KG     float64   `json:"kg"`
	Raw    string    `json:"raw"`
	Unit   string    `json:"unit"`
	Stable bool      `json:"stable"`
	TS     time.Time `json:"ts"`
}

// Stats zählt für die Statusseite.
type Stats struct {
	Reads    uint64 `json:"reads"`
	Timeouts uint64 `json:"timeouts"`
	BadLines uint64 `json:"bad_lines"`
	Reopens  uint64 `json:"reopens"`
	LastBad  string `json:"last_bad,omitempty"`
}

// Opener öffnet den Port; Tests ersetzen ihn.
type Opener func() (serialport.Port, string, error)

type request struct {
	cmd  string
	resp chan result
}

type result struct {
	val any
	err error
}

// Scale ist ein laufender Waagentreiber.
type Scale struct {
	cfg     config.DeviceConfig
	bus     *device.Bus
	log     *slog.Logger
	st      *device.StatusHolder
	open    Opener
	reqs    chan request
	wake    chan struct{}
	demand  atomic.Int32
	statsMu sync.Mutex
	stats   Stats
	stab    stability
}

// New ist die Factory für den Manager.
func New(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	return NewWithOpener(cfg, bus, log, func() (serialport.Port, string, error) {
		return serialport.Open(cfg.Port, cfg.Baud)
	})
}

// NewWithOpener erlaubt Tests eine Attrappe statt des echten Ports.
func NewWithOpener(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, open Opener) *Scale {
	return &Scale{
		cfg:  cfg,
		bus:  bus,
		log:  log,
		st:   device.NewStatusHolder(bus, cfg.ID, cfg.Kind, cfg.Driver, cfg.Port.String()),
		open: open,
		reqs: make(chan request),
		wake: make(chan struct{}, 1),
		stab: stability{n: cfg.StableSamples, tol: cfg.StableToleranceKG},
	}
}

func (s *Scale) ID() string   { return s.cfg.ID }
func (s *Scale) Kind() string { return "scale" }

func (s *Scale) Status() device.Status {
	st := s.st.Get()
	s.statsMu.Lock()
	st.Stats = s.stats
	s.statsMu.Unlock()
	return st
}

func (s *Scale) bump(fn func(*Stats)) {
	s.statsMu.Lock()
	fn(&s.stats)
	s.statsMu.Unlock()
}

// AddDemand zählt Zuhörer. Mit mindestens einem wird im poll_ms-Takt
// gemessen, sonst nur alle paar Sekunden als Lebenszeichen.
func (s *Scale) AddDemand(delta int) {
	if n := s.demand.Add(int32(delta)); n > 0 && n-int32(delta) <= 0 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Handle führt read oder tare aus.
func (s *Scale) Handle(ctx context.Context, cmd string, _ json.RawMessage) (any, error) {
	switch cmd {
	case "read", "tare":
	default:
		return nil, device.Errf("unknown_method", "scale."+cmd+" gibt es nicht")
	}
	ctx, cancel := context.WithTimeout(ctx, requestDeadline)
	defer cancel()
	r := request{cmd: cmd, resp: make(chan result, 1)}
	select {
	case s.reqs <- r:
	case <-ctx.Done():
		return nil, device.Errf("busy", "Waage ist beschäftigt")
	}
	select {
	case res := <-r.resp:
		return res.val, res.err
	case <-ctx.Done():
		return nil, device.Errf("timeout", "Waage hat nicht rechtzeitig geantwortet")
	}
}

// Run hält die Verbindung, bis ctx endet.
func (s *Scale) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		port, path, err := s.open()
		if err != nil {
			msg := err.Error()
			if errors.Is(err, serialport.ErrNotFound) {
				msg = "Waage nicht angeschlossen (" + s.cfg.Port.String() + ")"
			}
			s.st.Set(device.StateOffline, msg)
			s.waitOffline(ctx, backoff, msg)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		s.st.Update(func(st *device.Status) { st.Port = path })
		s.log.Info("Waage geöffnet", "path", path, "baud", s.cfg.Baud)
		err = s.session(ctx, port)
		_ = port.Close()
		if ctx.Err() != nil {
			return
		}
		s.bump(func(x *Stats) { x.Reopens++ })
		s.log.Warn("Waage-Verbindung neu aufbauen", "err", err)
		s.st.Set(device.StateOffline, "Verbindung verloren: "+err.Error())
		backoff = time.Second
		s.waitOffline(ctx, backoff, "Verbindung wird neu aufgebaut")
	}
}

// waitOffline wartet die Backoff-Zeit ab und beantwortet Anfragen sofort
// mit scale_offline statt sie hängen zu lassen.
func (s *Scale) waitOffline(ctx context.Context, d time.Duration, msg string) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case r := <-s.reqs:
			r.resp <- result{err: device.Errf("scale_offline", msg)}
		}
	}
}

func (s *Scale) session(ctx context.Context, port serialport.Port) error {
	s.stab.reset()
	timeouts, bad := 0, 0
	timer := time.NewTimer(0)
	defer timer.Stop()

	measure := func() (Reading, error) {
		rd, err := s.measure(port)
		var de *device.Error
		switch {
		case err == nil:
			timeouts, bad = 0, 0
			s.st.Set(device.StateOnline, "")
		case errors.As(err, &de) && de.Code == "timeout":
			timeouts++
			s.bump(func(x *Stats) { x.Timeouts++ })
			s.st.Set(device.StateError, "Waage antwortet nicht (eingeschaltet? Baudrate?)")
		case errors.As(err, &de) && de.Code == "bad_response":
			bad++
			s.bump(func(x *Stats) { x.BadLines++ })
			if bad >= 3 {
				s.st.Set(device.StateError, de.Message)
			}
		}
		return rd, err
	}
	next := func() time.Duration {
		if s.demand.Load() > 0 {
			return time.Duration(s.cfg.PollMS) * time.Millisecond
		}
		return idleInterval
	}
	fatal := func(err error) bool {
		var de *device.Error
		return err != nil && !errors.As(err, &de)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
			if _, err := measure(); fatal(err) {
				return err
			}
			timer.Reset(next())
		case <-timer.C:
			if _, err := measure(); fatal(err) {
				return err
			}
			timer.Reset(next())
		case r := <-s.reqs:
			var res result
			switch r.cmd {
			case "read":
				rd, err := measure()
				res = result{val: rd, err: err}
			case "tare":
				res = result{val: map[string]bool{"ok": true}, err: s.tare(port)}
			}
			if fatal(res.err) {
				r.resp <- result{err: device.Errf("scale_offline", "Verbindung zur Waage verloren")}
				return res.err
			}
			r.resp <- res
		}
		if timeouts >= maxTimeouts {
			return fmt.Errorf("%d Zeitüberschreitungen in Folge", timeouts)
		}
	}
}

// measure sendet Sx und wertet die Antwort aus. device.Error = Messproblem,
// andere Fehler = Port kaputt (neu öffnen).
func (s *Scale) measure(port serialport.Port) (Reading, error) {
	_ = port.ResetInputBuffer()
	if _, err := port.Write([]byte("Sx\r\n")); err != nil {
		return Reading{}, err
	}
	line, err := readLine(port, readTimeout)
	if err != nil {
		return Reading{}, err
	}
	if line == "" {
		return Reading{}, device.Errf("timeout", "Waage hat nicht innerhalb von 2 s geantwortet")
	}
	kg, unit, perr := ParseReading(line)
	if perr != nil {
		s.bump(func(x *Stats) { x.LastBad = line })
		return Reading{}, device.Errf("bad_response", perr.Error())
	}
	s.bump(func(x *Stats) { x.Reads++ })
	rd := Reading{KG: kg, Raw: line, Unit: unit, Stable: s.stab.add(kg), TS: time.Now().UTC()}
	s.st.Update(func(st *device.Status) { st.Last = rd })
	s.bus.Publish(device.Event{Name: "scale.weight", Device: s.cfg.ID, Data: rd})
	return rd, nil
}

func (s *Scale) tare(port serialport.Port) error {
	if _, err := port.Write([]byte("ST\r\n")); err != nil {
		return err
	}
	// Ob die Waage auf ST antwortet, ist nicht dokumentiert: kurz warten und
	// eine eventuelle Antwort verwerfen, damit sie nicht als Gewicht gilt.
	time.Sleep(300 * time.Millisecond)
	_ = port.ResetInputBuffer()
	s.stab.reset()
	s.log.Info("Waage tariert")
	return nil
}

// readLine liest bis CR oder LF. Leere Zeilen (das LF nach einem CR) werden
// übersprungen. Leerer String ohne Fehler = Zeitüberschreitung.
func readLine(port serialport.Port, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var buf bytes.Buffer
	chunk := make([]byte, 64)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return "", nil
		}
		if err := port.SetReadTimeout(min(left, 200*time.Millisecond)); err != nil {
			return "", err
		}
		n, err := port.Read(chunk)
		if err != nil {
			return "", err
		}
		for _, b := range chunk[:n] {
			if b == '\r' || b == '\n' {
				if buf.Len() > 0 {
					return buf.String(), nil
				}
				continue
			}
			buf.WriteByte(b)
			if buf.Len() > 256 {
				return buf.String(), nil // Müll; der Parser lehnt ihn ab
			}
		}
	}
}
