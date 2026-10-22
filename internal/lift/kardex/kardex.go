// Package kardex steuert einen Kardex-Remstar-Lift mit C3000-Steuerung
// (Megamat = VCM, vertikales Umlaufregal) über die HOST-CGI-Schnittstelle.
//
// Das Desk sagt »Träger 21« (lift.fetch); die Bridge schickt DriveTray und
// meldet erst zurück, wenn der Träger in der Bedienöffnung steht
// (GetDoorStatus: CurShelfBase = 21, ShelfBasePosFlag = 1, Maschine nicht
// beschäftigt). Unterwegs gehen lift.status-Events an Abonnenten, am Ziel
// lift.arrived.
//
// Voraussetzungen am Lift: Parameter 400.1 »UDP for Host« = OFF (sonst
// antwortet die Steuerung nicht auf CGI) und der Lift steht im Host-Menü
// (sonst 0xFFFB). Die Steuerung kann nur einen Host bedienen.
package kardex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// Kind ist die Geräteklasse.
const Kind = "lift"

const (
	// DefaultPort: der Webserver der C3000 lauscht für den Host auf 81.
	DefaultPort         = "81"
	defaultDriveTimeout = 120 * time.Second
	pollIdle            = 10 * time.Second
	pollWatched         = time.Second
	pollDriving         = 300 * time.Millisecond
	maxBackoff          = 15 * time.Second
	retryBusy           = 500 * time.Millisecond
)

var machineTypes = map[uint32]string{1: "Shuttle XP", 2: "Shuttle XPlus", 3: "Shuttle XPcrane", 4: "Puma", 5: "VCM (vertikales Umlaufregal)", 6: "HC (horizontales Umlaufregal)"}

// DoorStatus ist der Stand der Bedienöffnung (GetDoorStatus).
type DoorStatus struct {
	// Tray: Träger in der Bedienöffnung, 0 = keiner bzw. unterwegs.
	Tray       int  `json:"tray"`
	InPosition bool `json:"in_position"`
	Busy       bool `json:"busy"`
	// HostMenu false: der Lift steht nicht im Host-Menü, Fahrbefehle gehen
	// nicht. Die übrigen Felder sind dann ohne Bedeutung.
	HostMenu   bool      `json:"host_menu"`
	Automatic  bool      `json:"automatic,omitempty"`
	OpeningMsg int       `json:"opening_msg,omitempty"`
	MachineMsg int       `json:"machine_msg,omitempty"`
	TS         time.Time `json:"ts"`
}

func (s DoorStatus) same(o DoorStatus) bool {
	s.TS, o.TS = time.Time{}, time.Time{}
	return s == o
}

// Info beschreibt die Maschine (GetVersion, GetMachineInfo).
type Info struct {
	Version  string `json:"version,omitempty"`
	Type     string `json:"type,omitempty"`
	Trays    int    `json:"trays,omitempty"`
	Openings int    `json:"openings,omitempty"`
	WidthMM  int    `json:"width_mm,omitempty"`
	DepthMM  int    `json:"depth_mm,omitempty"`
}

// Last ist der Stand für Statusseite und Abonnenten.
type Last struct {
	Info
	Status *DoorStatus `json:"status,omitempty"`
	// Target: Träger, der gerade angefordert ist (0 = keiner).
	Target int `json:"target,omitempty"`
}

// Stats zählt Fahrten.
type Stats struct {
	Fetches   int    `json:"fetches"`
	Failures  int    `json:"failures,omitempty"`
	LastError string `json:"last_error,omitempty"`
	LastMS    int64  `json:"last_ms,omitempty"`
}

// Lift ist der Treiber.
type Lift struct {
	cfg          config.DeviceConfig
	bus          *device.Bus
	log          *slog.Logger
	st           *device.StatusHolder
	hi           uint32
	driveTimeout time.Duration
	// start liefert den Client, sobald Run läuft (Simulator: erst dann gibt
	// es die nachgebaute Steuerung).
	start func(ctx context.Context) (*Client, error)

	cli    atomic.Pointer[Client]
	demand atomic.Int32
	kick   chan struct{}

	mu    sync.Mutex
	last  Last
	stats Stats

	fetchMu sync.Mutex
	cur     *run // laufende Anforderung, nil = keine
}

// run ist eine Anforderung. Eine neue löst die laufende ab.
type run struct {
	tray   int
	cancel context.CancelCauseFunc
}

// New baut den Treiber für einen echten Lift.
func New(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	addr := Address(cfg.Address)
	return newLift(cfg, bus, log, addr, func(context.Context) (*Client, error) { return NewClient(addr), nil })
}

// Address ergänzt den Standardport 81.
func Address(a string) string {
	a = strings.TrimSpace(a)
	if _, _, err := net.SplitHostPort(a); err == nil {
		return a
	}
	return net.JoinHostPort(strings.Trim(a, "[]"), DefaultPort)
}

func newLift(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, port string, start func(context.Context) (*Client, error)) *Lift {
	hi := uint32(cfg.HostID)
	if hi == 0 {
		hi = 1
	}
	to := time.Duration(cfg.DriveTimeoutS) * time.Second
	if to == 0 {
		to = defaultDriveTimeout
	}
	return &Lift{
		cfg: cfg, bus: bus, log: log, hi: hi, driveTimeout: to, start: start,
		st:   device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, port),
		kick: make(chan struct{}, 1),
	}
}

func (l *Lift) ID() string   { return l.cfg.ID }
func (l *Lift) Kind() string { return Kind }

func (l *Lift) Status() device.Status {
	st := l.st.Get()
	l.mu.Lock()
	last := l.last
	if last.Status != nil {
		s := *last.Status
		last.Status = &s
	}
	st.Last, st.Stats = last, l.stats
	l.mu.Unlock()
	return st
}

// AddDemand: mit Zuschauer (Abonnent) wird der Stand öfter abgefragt.
func (l *Lift) AddDemand(delta int) {
	l.demand.Add(int32(delta))
	l.poke()
}

func (l *Lift) poke() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Run hält die Verbindung und fragt den Stand der Bedienöffnung ab.
func (l *Lift) Run(ctx context.Context) {
	cli, err := l.start(ctx)
	if err != nil {
		l.st.Set(device.StateError, err.Error())
		<-ctx.Done()
		return
	}
	l.cli.Store(cli)
	backoff := time.Second
	for ctx.Err() == nil {
		if err := l.connect(ctx, cli); err != nil {
			if ctx.Err() != nil {
				return
			}
			l.st.Set(device.StateOffline, "Lift nicht erreichbar: "+err.Error())
			l.log.Debug("Lift nicht erreichbar", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		l.watch(ctx, cli)
	}
}

func (l *Lift) connect(ctx context.Context, cli *Client) error {
	var info Info
	v, err := cli.Exec(ctx, cmdGetVersion, nil, []Tag{tagVersion})
	if err != nil && !isStatus(err) {
		return err
	}
	if x, ok := v.Get(tagVersion); ok {
		// Byteweise Haupt-, Neben-, Unterversion (Handbuch: 0x010d00 = 1.13.0).
		info.Version = fmt.Sprintf("%d.%d.%d", x>>16&0xff, x>>8&0xff, x&0xff)
	}
	v, err = cli.Exec(ctx, cmdGetMachineInfo, nil, []Tag{tagType, tagNumOpenings, tagNumTrays, tagWidth, tagDepth})
	if err != nil && !isStatus(err) {
		return err
	}
	if x, ok := v.Get(tagType); ok {
		info.Type = machineTypes[x]
		if info.Type == "" {
			info.Type = fmt.Sprintf("Typ %d", x)
		}
	}
	if x, ok := v.Get(tagNumTrays); ok {
		info.Trays = int(x)
	}
	if x, ok := v.Get(tagNumOpenings); ok {
		info.Openings = int(x)
	}
	if x, ok := v.Get(tagWidth); ok {
		info.WidthMM = int(x)
	}
	if x, ok := v.Get(tagDepth); ok {
		info.DepthMM = int(x)
	}
	l.mu.Lock()
	l.last.Info = info
	l.mu.Unlock()
	l.log.Info("Lift verbunden", "type", info.Type, "trays", info.Trays, "version", info.Version)
	return nil
}

// watch fragt den Stand ab, bis die Verbindung abreißt.
func (l *Lift) watch(ctx context.Context, cli *Client) {
	for ctx.Err() == nil {
		if _, err := l.refresh(ctx, cli); err != nil {
			if ctx.Err() != nil {
				return
			}
			l.st.Set(device.StateOffline, "Lift nicht erreichbar: "+err.Error())
			return
		}
		wait := pollIdle
		if l.demand.Load() > 0 {
			wait = pollWatched
		}
		select {
		case <-ctx.Done():
			return
		case <-l.kick:
		case <-time.After(wait):
		}
	}
}

// refresh liest den Stand der Bedienöffnung, setzt den Gerätezustand und
// meldet Änderungen als lift.status.
func (l *Lift) refresh(ctx context.Context, cli *Client) (DoorStatus, error) {
	ds, err := l.doorStatus(ctx, cli)
	if err != nil {
		return ds, err
	}
	switch {
	case !ds.HostMenu:
		l.st.Set(device.StateOnline, "Lift ist nicht im Host-Menü – Fahrbefehle gehen erst dort")
	case ds.MachineMsg != 0 || ds.OpeningMsg != 0:
		l.st.Set(device.StateOnline, fmt.Sprintf("Meldung am Lift: %s", msgText(ds)))
	default:
		l.st.Set(device.StateOnline, "")
	}
	l.mu.Lock()
	changed := l.last.Status == nil || !l.last.Status.same(ds)
	l.last.Status = &ds
	target := l.last.Target
	l.mu.Unlock()
	if changed {
		l.bus.Publish(device.Event{Name: "lift.status", Device: l.cfg.ID, Data: statusEvent{ds, target}})
	}
	return ds, nil
}

type statusEvent struct {
	DoorStatus
	Target int `json:"target,omitempty"`
}

func msgText(ds DoorStatus) string {
	var p []string
	if ds.OpeningMsg != 0 {
		p = append(p, fmt.Sprintf("Öffnung %04d", ds.OpeningMsg))
	}
	if ds.MachineMsg != 0 {
		p = append(p, fmt.Sprintf("Maschine %04d", ds.MachineMsg))
	}
	return strings.Join(p, ", ")
}

func (l *Lift) doorStatus(ctx context.Context, cli *Client) (DoorStatus, error) {
	in := []Val{valid(tagHI, l.hi), unset(tagLevelLower), unset(tagLevelUpper)}
	out := []Tag{tagMachineBusy, tagCurTray, tagPosFlag, tagOpeningMsg, tagMachineMsg, tagActiveHI, tagOpeningBusy, tagAutomaticMode}
	v, err := cli.Exec(ctx, cmdGetDoorStatus, in, out)
	ds := DoorStatus{TS: time.Now(), HostMenu: true}
	var se *StatusError
	if errors.As(err, &se) && se.Status == retNotHostMenu {
		ds.HostMenu = false
		return ds, nil
	}
	if err != nil {
		return ds, err
	}
	busy, _ := v.Get(tagMachineBusy)
	ob, _ := v.Get(tagOpeningBusy)
	tray, _ := v.Get(tagCurTray)
	pos, _ := v.Get(tagPosFlag)
	auto, _ := v.Get(tagAutomaticMode)
	om, _ := v.Get(tagOpeningMsg)
	mm, _ := v.Get(tagMachineMsg)
	ds.Busy = busy == 1 || ob == 1
	ds.Tray = int(tray)
	ds.InPosition = pos == 1
	ds.Automatic = auto == 1
	ds.OpeningMsg, ds.MachineMsg = int(om), int(mm)
	return ds, nil
}

func isStatus(err error) bool {
	var se *StatusError
	return errors.As(err, &se)
}

// --- Kommandos ---

type fetchParams struct {
	Tray int `json:"tray"`
	// Positionsanzeige am Lift (Lampen), z.B. Fach 3–4 auf dem Träger.
	LightFrom int `json:"light_from"`
	LightTo   int `json:"light_to"`
	// Wait false: nur losschicken, Ankunft kommt als lift.arrived.
	Wait     *bool `json:"wait"`
	TimeoutS int   `json:"timeout_s"`
}

var errSuperseded = errors.New("superseded")

// Handle führt ein Kommando aus.
func (l *Lift) Handle(ctx context.Context, cmd string, raw json.RawMessage) (any, error) {
	cli := l.cli.Load()
	if cli == nil {
		return nil, device.Errf("device_offline", "Lift startet noch")
	}
	switch cmd {
	case "fetch":
		var p fetchParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, device.Errf("bad_request", "Parameter nicht lesbar")
			}
		}
		return l.fetch(ctx, cli, p)
	case "status", "read":
		ds, err := l.refresh(ctx, cli)
		if err != nil {
			return nil, l.fail(err)
		}
		l.mu.Lock()
		info := l.last.Info
		l.mu.Unlock()
		return map[string]any{"info": info, "status": ds}, nil
	case "confirm", "cancel":
		c := uint16(cmdConfirm)
		if cmd == "cancel" {
			c = cmdCancel
		}
		if _, err := cli.Exec(ctx, c, []Val{valid(tagHI, l.hi)}, nil); err != nil {
			return nil, l.fail(err)
		}
		l.poke()
		return map[string]any{"ok": true}, nil
	case "display":
		var p struct {
			Lines []string `json:"lines"`
		}
		_ = json.Unmarshal(raw, &p)
		if len(p.Lines) > 4 {
			return nil, device.Errf("bad_request", "höchstens 4 Zeilen")
		}
		in := []Val{valid(tagHI, l.hi)}
		for i := 0; i < 4; i++ {
			s := ""
			if i < len(p.Lines) {
				s = p.Lines[i]
			}
			in = append(in, text(alphaText[i], s, 20))
		}
		if _, err := cli.Exec(ctx, cmdSetAlphaText, in, nil); err != nil {
			return nil, l.fail(err)
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, device.Errf("unknown_command", "lift."+cmd+" gibt es nicht")
}

var alphaText = []Tag{b(154), b(176), b(198), b(220)}

func (l *Lift) fetch(ctx context.Context, cli *Client, p fetchParams) (any, error) {
	l.mu.Lock()
	trays := l.last.Trays
	l.mu.Unlock()
	if p.Tray < 1 || (trays > 0 && p.Tray > trays) {
		if trays > 0 {
			return nil, device.Errf("bad_request", fmt.Sprintf("Träger %d gibt es nicht (1–%d)", p.Tray, trays))
		}
		return nil, device.Errf("bad_request", "tray fehlt")
	}
	if p.LightFrom < 0 || p.LightTo < 0 || p.LightFrom > 0x7fff || p.LightTo > 0x7fff {
		return nil, device.Errf("bad_request", "light_from/light_to ungültig")
	}
	timeout := l.driveTimeout
	if p.TimeoutS > 0 {
		timeout = time.Duration(p.TimeoutS) * time.Second
	}
	wait := p.Wait == nil || *p.Wait

	// Eine neue Anforderung löst die alte ab: deren Aufrufer bekommt
	// »superseded«, der Lift fährt zum neuen Ziel.
	var fctx context.Context
	var cancel context.CancelCauseFunc
	if wait {
		fctx, cancel = context.WithCancelCause(ctx)
	} else {
		fctx, cancel = context.WithCancelCause(context.Background())
	}
	me := &run{tray: p.Tray, cancel: cancel}
	l.fetchMu.Lock()
	if l.cur != nil {
		l.cur.cancel(errSuperseded)
	}
	l.cur = me
	l.fetchMu.Unlock()

	started := time.Now()
	l.mu.Lock()
	l.last.Target = p.Tray
	l.stats.Fetches++
	l.mu.Unlock()
	l.log.Info("Träger angefordert", "tray", p.Tray)

	if err := l.drive(fctx, cli, p); err != nil {
		l.done(me, started, err)
		return nil, l.fetchErr(fctx, p.Tray, timeout, err)
	}
	if !wait {
		go func() {
			err := l.awaitArrival(fctx, cli, p.Tray, timeout, started)
			l.done(me, started, err)
		}()
		return map[string]any{"tray": p.Tray, "accepted": true}, nil
	}
	err := l.awaitArrival(fctx, cli, p.Tray, timeout, started)
	l.done(me, started, err)
	if err != nil {
		return nil, l.fetchErr(fctx, p.Tray, timeout, err)
	}
	return map[string]any{"tray": p.Tray, "arrived": true, "ms": time.Since(started).Milliseconds()}, nil
}

// drive schickt DriveTray. Läuft noch ein Befehl, wird kurz wiederholt.
func (l *Lift) drive(ctx context.Context, cli *Client, p fetchParams) error {
	in := []Val{
		valid(tagHI, l.hi),
		lamp(tagPosLightFrom, p.LightFrom), lamp(tagPosLightTo, p.LightTo),
		unset(tagDepthLightFrom), unset(tagDepthLightTo),
		valid(tagTrayLower, uint32(p.Tray)),
		unset(tagLevelLower), unset(tagTrayUpper), unset(tagLevelUpper), unset(tagTrayLift), unset(tagLevelLift),
	}
	for {
		_, err := cli.Exec(ctx, cmdDriveTray, in, nil)
		var se *StatusError
		retry := errors.Is(err, ErrBusy) || errors.As(err, &se) && (se.Status == 25 || se.Status == retHostBusy)
		if !retry {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryBusy):
		}
	}
}

func lamp(t Tag, v int) Val {
	if v <= 0 {
		return unset(t)
	}
	return valid(t, uint32(v))
}

// awaitArrival pollt die Bedienöffnung, bis der Träger in Position steht.
func (l *Lift) awaitArrival(ctx context.Context, cli *Client, tray int, timeout time.Duration, started time.Time) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		ds, err := l.refresh(ctx, cli)
		if err == nil && !ds.HostMenu {
			return &StatusError{Cmd: cmdGetDoorStatus, Status: retNotHostMenu}
		}
		if err == nil && ds.Tray == tray && ds.InPosition && !ds.Busy {
			ms := time.Since(started).Milliseconds()
			l.log.Info("Träger in der Bedienöffnung", "tray", tray, "ms", ms)
			l.bus.Publish(device.Event{Name: "lift.arrived", Device: l.cfg.ID, Data: map[string]any{"tray": tray, "ms": ms, "ts": time.Now()}})
			return nil
		}
		if err != nil && ctx.Err() == nil {
			l.log.Debug("Stand während der Fahrt nicht lesbar", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-time.After(pollDriving):
		}
	}
}

func (l *Lift) done(me *run, started time.Time, err error) {
	me.cancel(nil)
	l.fetchMu.Lock()
	current := l.cur == me
	if current {
		l.cur = nil
	}
	l.fetchMu.Unlock()
	superseded := !current
	l.mu.Lock()
	if current {
		l.last.Target = 0
	}
	switch {
	case err == nil:
		l.stats.LastMS = time.Since(started).Milliseconds()
	case !superseded:
		l.stats.Failures++
		l.stats.LastError = err.Error()
	}
	l.mu.Unlock()
	if err != nil && !superseded {
		l.bus.Publish(device.Event{Name: "lift.failed", Device: l.cfg.ID, Data: map[string]any{"tray": me.tray, "error": l.fail(err).Error(), "ts": time.Now()}})
	}
}

func (l *Lift) fetchErr(ctx context.Context, tray int, timeout time.Duration, err error) error {
	if errors.Is(context.Cause(ctx), errSuperseded) {
		return device.Errf("superseded", fmt.Sprintf("Träger %d: inzwischen wurde ein anderer Träger angefordert", tray))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return device.Errf("timeout", fmt.Sprintf("Träger %d ist nach %d s nicht in der Bedienöffnung", tray, int(timeout.Seconds())))
	}
	return l.fail(err)
}

// fail übersetzt einen Fehler in einen device.Error.
func (l *Lift) fail(err error) error {
	var se *StatusError
	switch {
	case errors.As(err, &se) && se.Status == retNotHostMenu:
		return device.Errf("not_host_menu", "Der Lift ist nicht im Host-Menü. Am Bedienteil ins Host-Menü wechseln.")
	case errors.As(err, &se):
		return device.Errf("lift_error", StatusText(se.Cmd, se.Status))
	case errors.Is(err, ErrBusy):
		return device.Errf("lift_busy", "Die Steuerung bearbeitet noch ein Kommando.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return device.Errf("timeout", "Lift hat nicht rechtzeitig geantwortet")
	}
	return device.Errf("device_offline", "Lift nicht erreichbar: "+err.Error())
}
