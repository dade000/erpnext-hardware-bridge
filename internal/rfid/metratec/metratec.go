// Package metratec ist der Treiber für UHF-RFID-Reader von metratec mit
// AT-Protokoll, gebaut und gedacht für den DeskID UHF v2 (USB-C, virtueller
// COM-Port, 115200 Baud).
//
// Aufgabe an der Fotostation: den einen Tag am Paar erkennen (TID) und seinen
// NFC-Teil so beschreiben, dass ein Handy eine Adresse öffnet. Dafür braucht
// es Dual-Frequenz-Tags wie EM4425, bei denen UHF und NFC denselben Speicher
// sehen.
package metratec

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/serialport"
)

// Kind ist die Geräteklasse.
const Kind = "rfid"

var hfStarts = []int{320, 160}

const (
	cmdTimeout      = 2 * time.Second
	invTimeout      = 4 * time.Second
	writeTimeout    = 5 * time.Second
	idleInterval    = 5 * time.Second // Lebenszeichen ohne Zuhörer
	maxTimeouts     = 3               // danach Port neu öffnen
	maxBackoff      = 5 * time.Second
	requestDeadline = 30 * time.Second
	// writeChunk: so viele Bytes je AT+WRT. Die Obergrenze des Readers steht
	// nicht im SDK; 16 Byte sind sicher klein und kosten nur ein paar
	// Funkbefehle mehr.
	writeChunk = 16
	// dumpLen: so viel Nutzerspeicher liest der Test je Befehl; am Ende des
	// Speichers wird halbiert, bis nichts mehr geht.
	dumpLen = 32
	// maxRead: mehr Byte nimmt der DeskID UHF v2 je AT+READ nicht an.
	maxRead = 32
	// dumpMax: weiter liest der Test nicht. Der EM4425 hat 2048 bit
	// Gesamtspeicher; 512 Byte decken jede Aufteilung ab.
	dumpMax = 256
	// hfStarts: Startadressen, unter denen der NFC-Bereich des EM4425 in der
	// UHF-Nutzerbank liegt (Wort A0h, als Byte- bzw. Wortadresse).
	// hfMax: so viel wird dort höchstens gelesen.
	hfMax = 256
	// tidBytes: so lang ist eine eindeutige TID (96 bit, Gen2 mit XTID).
	tidBytes = 12
)

// Stats zählt für die Statusseite.
type Stats struct {
	Inventories uint64 `json:"inventories"`
	Writes      uint64 `json:"writes"`
	Timeouts    uint64 `json:"timeouts"`
	Reopens     uint64 `json:"reopens"`
	LastError   string `json:"last_error,omitempty"`
}

// Last ist das letzte Inventory (für Statusseite und Abonnenten).
type Last struct {
	Tags []Tag     `json:"tags"`
	TS   time.Time `json:"ts"`
}

// Opener öffnet den Port; Tests ersetzen ihn.
type Opener func() (serialport.Port, string, error)

type request struct {
	cmd    string
	params json.RawMessage
	resp   chan result
}

type result struct {
	val any
	err error
}

// Reader ist ein laufender Treiber.
type Reader struct {
	cfg    config.DeviceConfig
	bus    *device.Bus
	log    *slog.Logger
	st     *device.StatusHolder
	open   Opener
	reqs   chan request
	wake   chan struct{}
	demand atomic.Int32
	// resend: nächstes Inventory auf jeden Fall als rfid.tags melden, auch
	// ohne Änderung – ein neuer Zuhörer kennt den Stand sonst nicht.
	resend  atomic.Bool
	statsMu sync.Mutex
	stats   Stats
	info    readerInfo
	lastKey string
}

// New ist die Factory für den Manager.
func New(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	return NewWithOpener(cfg, bus, log, func() (serialport.Port, string, error) {
		return serialport.Open(cfg.Port, cfg.Baud)
	})
}

// NewWithOpener erlaubt Tests eine Attrappe statt des echten Ports.
func NewWithOpener(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, open Opener) *Reader {
	return &Reader{
		cfg:  cfg,
		bus:  bus,
		log:  log,
		st:   device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, cfg.Port.String()),
		open: open,
		reqs: make(chan request),
		wake: make(chan struct{}, 1),
	}
}

func (r *Reader) ID() string   { return r.cfg.ID }
func (r *Reader) Kind() string { return Kind }

func (r *Reader) Status() device.Status {
	st := r.st.Get()
	r.statsMu.Lock()
	st.Stats = r.stats
	r.statsMu.Unlock()
	return st
}

func (r *Reader) bump(fn func(*Stats)) {
	r.statsMu.Lock()
	fn(&r.stats)
	r.statsMu.Unlock()
}

// AddDemand zählt Zuhörer. Mit mindestens einem läuft im poll_ms-Takt ein
// Inventory, damit die Fotostation sieht, ob (genau) ein Tag aufliegt.
func (r *Reader) AddDemand(delta int) {
	if delta > 0 {
		r.resend.Store(true)
	}
	if n := r.demand.Add(int32(delta)); n > 0 && n-int32(delta) <= 0 {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Handle: read (Inventory, bei genau einem Tag mit Speicherauszug) und
// write_uri.
func (r *Reader) Handle(ctx context.Context, cmd string, params json.RawMessage) (any, error) {
	switch cmd {
	case "read", "inventory", "write_uri":
	default:
		return nil, device.Errf("unknown_method", "rfid."+cmd+" gibt es nicht")
	}
	ctx, cancel := context.WithTimeout(ctx, requestDeadline)
	defer cancel()
	q := request{cmd: cmd, params: params, resp: make(chan result, 1)}
	select {
	case r.reqs <- q:
	case <-ctx.Done():
		return nil, device.Errf("busy", "RFID-Reader ist beschäftigt")
	}
	select {
	case res := <-q.resp:
		return res.val, res.err
	case <-ctx.Done():
		return nil, device.Errf("timeout", "RFID-Reader hat nicht rechtzeitig geantwortet")
	}
}

// Run hält die Verbindung, bis ctx endet.
func (r *Reader) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		port, path, err := r.open()
		if err != nil {
			msg := err.Error()
			if errors.Is(err, serialport.ErrNotFound) {
				msg = "RFID-Reader nicht angeschlossen (" + r.cfg.Port.String() + ")"
			}
			r.st.Set(device.StateOffline, msg)
			r.waitOffline(ctx, backoff, msg)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		r.st.Update(func(st *device.Status) { st.Port = path })
		r.log.Info("RFID-Reader geöffnet", "path", path, "baud", r.cfg.Baud)
		err = r.session(ctx, &atConn{port: port})
		_ = port.Close()
		if ctx.Err() != nil {
			return
		}
		r.bump(func(x *Stats) { x.Reopens++ })
		r.log.Warn("RFID-Verbindung neu aufbauen", "err", err)
		if fatal(err) {
			r.st.Set(device.StateOffline, "Verbindung verloren: "+err.Error())
			backoff = time.Second
		} else {
			// Reader antwortet, aber falsch (Einrichtung, falsches Gerät,
			// Zeitüberschreitungen): Zustand "error" stehen lassen und
			// seltener neu versuchen.
			backoff = maxBackoff
		}
		r.waitOffline(ctx, backoff, "Verbindung wird neu aufgebaut")
	}
}

func (r *Reader) waitOffline(ctx context.Context, d time.Duration, msg string) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case q := <-r.reqs:
			q.resp <- result{err: device.Errf("rfid_offline", msg)}
		}
	}
}

// setup bringt den Reader in einen bekannten Zustand: kein Echo, kein
// laufendes Dauer-Inventory, Inventory mit TID und RSSI, Sendeleistung.
func (r *Reader) setup(c *atConn) error {
	// ATE0 schaltet das Echo ab; command übergeht ein Echo aber ohnehin.
	if _, err := c.command("ATE0", cmdTimeout); err != nil {
		return err
	}
	lines, err := c.command("ATI", cmdTimeout)
	if err != nil {
		return err
	}
	r.info = parseInfo(lines)
	if hw := strings.ToLower(r.info.Hardware); hw != "" && !strings.Contains(hw, "uhf") {
		return device.Errf("wrong_reader", "Kein UHF-Reader: "+r.info.Hardware)
	}
	// Ein Dauer-Inventory aus einer früheren Sitzung beenden. Läuft keins,
	// antwortet der Reader mit ERROR – das ist hier kein Fehler.
	if _, err := c.command("AT+BINV", cmdTimeout); err != nil && !isATError(err) {
		return err
	}
	lines, err = c.command("AT+INVS?", cmdTimeout)
	if err != nil {
		return err
	}
	// TID mit 12 Byte anfordern: die Vorgabe der DeskID-Firmware liefert nur
	// 8, und die ersten 8 Byte eines EM4425 (Klasse, Hersteller, Modell, Teil
	// der Seriennummer) sind nicht eindeutig. Ältere Firmware kennt nur 0/1.
	set, err := invSettings(lines, strconv.Itoa(tidBytes))
	if err != nil {
		return device.Errf("bad_response", err.Error())
	}
	if _, err := c.command(set, cmdTimeout); err != nil {
		if !isATError(err) {
			return err
		}
		r.log.Info("TID-Länge im Inventory nicht einstellbar, lese sie bei Bedarf nach", "err", err)
		set, _ = invSettings(lines, "1")
		if _, err := c.command(set, cmdTimeout); err != nil {
			return err
		}
	}
	if r.cfg.PowerDBm > 0 {
		if _, err := c.command("AT+PWR="+strconv.Itoa(r.cfg.PowerDBm), cmdTimeout); err != nil {
			return err
		}
	}
	return nil
}

func isATError(err error) bool {
	var ae *atError
	return errors.As(err, &ae)
}

// fatal: Fehler, nach denen der Port neu geöffnet wird.
func fatal(err error) bool {
	if err == nil || isATError(err) {
		return false
	}
	var de *device.Error
	return !errors.As(err, &de)
}

func (r *Reader) session(ctx context.Context, c *atConn) error {
	if err := r.setup(c); err != nil {
		if !fatal(err) {
			r.st.Set(device.StateError, "Einrichtung fehlgeschlagen: "+err.Error())
		}
		return err
	}
	r.st.Set(device.StateOnline, strings.TrimSpace(r.info.Firmware))
	r.log.Info("RFID-Reader bereit", "firmware", r.info.Firmware, "hardware", r.info.Hardware, "serial", r.info.Serial)

	timeouts := 0
	note := func(err error) {
		var de *device.Error
		switch {
		case err == nil:
			timeouts = 0
			r.st.Set(device.StateOnline, strings.TrimSpace(r.info.Firmware))
		case errors.As(err, &de) && de.Code == "timeout":
			timeouts++
			r.bump(func(x *Stats) { x.Timeouts++ })
			r.st.Set(device.StateError, "RFID-Reader antwortet nicht")
		}
	}
	poll := func() error {
		var err error
		if r.demand.Load() > 0 {
			_, err = r.inventory(c)
		} else {
			_, err = c.command("AT", cmdTimeout)
		}
		note(err)
		return err
	}
	next := func() time.Duration {
		if r.demand.Load() > 0 {
			return time.Duration(r.cfg.PollMS) * time.Millisecond
		}
		return idleInterval
	}

	timer := time.NewTimer(next())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.wake:
			if err := poll(); fatal(err) {
				return err
			}
			timer.Reset(next())
		case <-timer.C:
			if err := poll(); fatal(err) {
				return err
			}
			timer.Reset(next())
		case q := <-r.reqs:
			val, err := r.do(c, q)
			note(timeoutOnly(err))
			if fatal(err) {
				q.resp <- result{err: device.Errf("rfid_offline", "Verbindung zum RFID-Reader verloren")}
				return err
			}
			if err != nil {
				r.bump(func(x *Stats) { x.LastError = err.Error() })
				var de *device.Error
				if !errors.As(err, &de) {
					err = device.Errf("reader_error", err.Error())
				}
			}
			q.resp <- result{val: val, err: err}
		}
		if timeouts >= maxTimeouts {
			return device.Errf("timeout", fmt.Sprintf("%d Zeitüberschreitungen in Folge", timeouts))
		}
	}
}

// timeoutOnly: für die Zustandsanzeige zählen nur Zeitüberschreitungen und
// Erfolg; "kein Tag" heißt nicht, dass der Reader krank ist.
func timeoutOnly(err error) error {
	var de *device.Error
	if err == nil || (errors.As(err, &de) && de.Code == "timeout") {
		return err
	}
	return nil
}

func (r *Reader) do(c *atConn, q request) (any, error) {
	switch q.cmd {
	case "inventory":
		tags, err := r.inventory(c)
		if err != nil {
			return nil, err
		}
		return map[string]any{"tags": tags}, nil
	case "read":
		return r.readDump(c)
	case "write_uri":
		var p writeParams
		if len(q.params) > 0 {
			if err := json.Unmarshal(q.params, &p); err != nil {
				return nil, device.Errf("bad_request", "Parameter nicht lesbar: "+err.Error())
			}
		}
		return r.writeURI(c, p)
	}
	return nil, device.Errf("unknown_method", q.cmd)
}

// inventory liest alle Tags im Feld und meldet Änderungen auf dem Bus.
func (r *Reader) inventory(c *atConn) ([]Tag, error) {
	lines, err := c.command("AT+INV", invTimeout)
	if err != nil {
		return nil, err
	}
	tags, err := parseInventory(lines)
	if err != nil {
		return nil, err
	}
	r.bump(func(x *Stats) { x.Inventories++ })
	slices.SortFunc(tags, func(a, b Tag) int { return strings.Compare(a.TID+a.EPC, b.TID+b.EPC) })
	last := Last{Tags: tags, TS: time.Now().UTC()}
	r.st.Update(func(st *device.Status) { st.Last = last })
	// Nur Änderungen verschicken: die Fotostation will wissen, wann ein Tag
	// aufgelegt oder weggenommen wird, nicht viermal pro Sekunde dasselbe.
	key := ""
	for _, t := range tags {
		key += t.TID + "/" + t.EPC + ";"
	}
	if key != r.lastKey || r.resend.Swap(false) {
		r.lastKey = key
		r.bus.Publish(device.Event{Name: "rfid.tags", Device: r.cfg.ID, Data: last})
	}
	return tags, nil
}

// single verlangt genau einen Tag mit lesbarer TID.
func (r *Reader) single(c *atConn) (Tag, error) {
	tags, err := r.inventory(c)
	if err != nil {
		return Tag{}, err
	}
	switch len(tags) {
	case 0:
		return Tag{}, device.Errf("no_tag", "Kein RFID-Tag am Reader.")
	case 1:
	default:
		return Tag{}, device.Errf("multiple_tags", fmt.Sprintf("%d RFID-Tags am Reader – bitte nur das eine Paar auflegen.", len(tags)))
	}
	tag := tags[0]
	if tag.TID == "" {
		return Tag{}, device.Errf("no_tid", "Die TID des Tags ließ sich nicht lesen – Tag näher an den Reader legen.")
	}
	full, err := r.fullTID(c, tag.TID)
	if err != nil {
		return Tag{}, err
	}
	tag.TID = full
	return tag, nil
}

// fullTID liest die TID auf volle 12 Byte nach, wenn das Inventory nur einen
// Anfang geliefert hat. Ohne eindeutige TID wird nichts beschrieben: die
// Adresse auf dem Tag und die Zuordnung zur Seriennummer hängen an ihr.
func (r *Reader) fullTID(c *atConn, tid string) (string, error) {
	if len(tid) >= 2*tidBytes {
		return tid, nil
	}
	var full string
	err := r.withMask(c, tid, func() error {
		res, err := r.tagCommand(c, fmt.Sprintf("AT+READ=TID,0,%d", tidBytes), "+READ: ")
		if err != nil {
			return err
		}
		if res.Status != "OK" || !isHex(res.Data) {
			return device.Errf("no_tid", "Die TID ließ sich nicht vollständig lesen: "+res.Status)
		}
		full = strings.ToUpper(res.Data)
		return nil
	})
	if err != nil {
		if isATError(err) {
			err = device.Errf("no_tid", "Die TID ließ sich nicht vollständig lesen: "+err.Error())
		}
		return "", err
	}
	if len(full) < 2*tidBytes || !strings.HasPrefix(full, tid) {
		return "", device.Errf("no_tid", "Die TID des Tags ist kürzer als 12 Byte und damit nicht eindeutig: "+full)
	}
	return full, nil
}

// withMask beschränkt alle folgenden Funkbefehle auf den Tag mit dieser TID
// und hebt die Beschränkung danach wieder auf.
func (r *Reader) withMask(c *atConn, tid string, fn func() error) error {
	if _, err := c.command("AT+MSK=TID,0,"+tid, cmdTimeout); err != nil {
		return err
	}
	err := fn()
	if _, merr := c.command("AT+MSK=OFF", cmdTimeout); merr != nil && err == nil {
		err = merr
	}
	return err
}

// readUSR liest Nutzerspeicher des maskierten Tags.
// tagRetries: so oft wird ein Lese- oder Schreibbefehl wiederholt, wenn der
// Tag in dieser Funkrunde nicht geantwortet hat. Am echten DeskID kam das bei
// gut aufliegendem Tag (-32 dBm) vor; beim nächsten Versuch war er wieder da.
const tagRetries = 3

// tagCommand schickt einen Befehl an den (maskierten) Tag und liefert seine
// Antwortzeile. Antwortet kein Tag, wird wiederholt.
func (r *Reader) tagCommand(c *atConn, cmd, prefix string) (tagResult, error) {
	for try := 1; ; try++ {
		lines, err := c.command(cmd, writeTimeout)
		if err != nil {
			return tagResult{}, err
		}
		if res := parseTagResults(lines, prefix); len(res) > 0 {
			return res[0], nil
		}
		if try >= tagRetries {
			return tagResult{}, device.Errf("no_tag", fmt.Sprintf("Tag hat %d-mal nicht geantwortet – bitte ruhig auflegen und nochmal.", tagRetries))
		}
		r.log.Debug("Tag hat nicht geantwortet, nochmal", "cmd", cmd, "try", try)
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *Reader) readUSR(c *atConn, start, length int) ([]byte, error) {
	res, err := r.tagCommand(c, fmt.Sprintf("AT+READ=USR,%d,%d", start, length), "+READ: ")
	if err != nil {
		return nil, err
	}
	if res.Status != "OK" {
		return nil, device.Errf("read_failed", "Lesen fehlgeschlagen: "+res.Status)
	}
	data, err := hex.DecodeString(res.Data)
	if err != nil {
		return nil, device.Errf("bad_response", "Speicherinhalt nicht lesbar: "+res.Data)
	}
	return data, nil
}

// readUSRChunked liest length Byte in Stücken, die der Reader annimmt: der
// DeskID UHF v2 lehnt mehr als 32 Byte je AT+READ mit "Read length too big" ab.
func (r *Reader) readUSRChunked(c *atConn, start, length int) ([]byte, error) {
	var out []byte
	for off := 0; off < length; off += maxRead {
		part, err := r.readUSR(c, start+off, min(maxRead, length-off))
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

// readDump: Inventory und – bei genau einem Tag – die ersten Bytes des
// Nutzerspeichers samt erkannter Adresse. Dient der Einrichtung: wer mit
// einer Handy-App eine Adresse auf den Tag schreibt, sieht hier, an welchem
// Offset der NFC-Bereich im UHF-Nutzerspeicher liegt.
func (r *Reader) readDump(c *atConn) (any, error) {
	tags, err := r.inventory(c)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tags": tags, "reader": r.info}
	if len(tags) != 1 || tags[0].TID == "" {
		return out, nil
	}
	full, err := r.fullTID(c, tags[0].TID)
	if fatal(err) {
		return nil, err
	}
	if err != nil {
		out["tid_error"] = err.Error()
		full = tags[0].TID
	} else {
		tags[0].TID = full
	}
	// Bereich 1: der UHF-Nutzerspeicher ab 0, stückweise bis zu seinem Ende.
	// Bereich 2: der NFC-Nutzerspeicher des EM4425. Er hängt nicht an den
	// UHF-Bereich an, sondern liegt in derselben Speicherbank ab Wort A0h
	// (Hinweis aus stackoverflow.com/q/78100511). Ob der Reader die
	// Startadresse in Byte (320) oder in Worten (160) nimmt, sagt das SDK nicht
	// verlässlich – beide werden versucht.
	var regions []memRegion
	err = r.withMask(c, full, func() error {
		reg, err := r.readRegion(c, 0, dumpMax)
		if err != nil {
			return err
		}
		regions = append(regions, reg)
		for _, st := range hfStarts {
			reg, err := r.readRegion(c, st, hfMax)
			if err != nil {
				return err
			}
			if len(reg.data) > 0 {
				regions = append(regions, reg)
				break
			}
			if st == hfStarts[len(hfStarts)-1] {
				regions = append(regions, reg)
			}
		}
		return nil
	})
	if fatal(err) {
		return nil, err
	}
	if err != nil {
		out["usr_error"] = err.Error()
		return out, nil
	}
	var list []map[string]any
	for _, reg := range regions {
		m := map[string]any{"start": reg.start, "bytes": len(reg.data), "hex": strings.ToUpper(hex.EncodeToString(reg.data))}
		if reg.end != nil {
			m["end"] = reg.end.Error()
		}
		list = append(list, m)
		if _, found := out["ndef"]; found || len(reg.data) == 0 {
			continue
		}
		if info, nerr := FindType5(reg.data); nerr == nil {
			info.Offset += reg.start
			out["ndef"] = info
		}
	}
	out["regions"] = list
	if _, found := out["ndef"]; !found {
		out["ndef_error"] = "keine Adresse in den gelesenen Bereichen"
	}
	return out, nil
}

// memRegion ist ein zusammenhängend lesbarer Teil des Nutzerspeichers.
type memRegion struct {
	start int
	data  []byte
	end   error // warum danach nicht weitergelesen wurde
}

// readRegion liest ab start stückweise, bis der Speicher endet oder max Byte
// gelesen sind. Nur Portfehler kommen als Fehler zurück.
func (r *Reader) readRegion(c *atConn, start, max int) (memRegion, error) {
	reg := memRegion{start: start}
	for off := start; off < start+max; {
		n := dumpLen
		var part []byte
		var err error
		for ; n >= 2; n /= 2 {
			part, err = r.readUSR(c, off, n)
			if err == nil || fatal(err) {
				break
			}
		}
		if fatal(err) {
			return reg, err
		}
		if err != nil {
			reg.end = err
			return reg, nil
		}
		reg.data = append(reg.data, part...)
		off += n
	}
	return reg, nil
}

type writeParams struct {
	// URI ist die Adresse; "{tid}" wird durch die TID des Tags ersetzt.
	URI string `json:"uri"`
	// Offset im UHF-Nutzerspeicher (Byte), an dem der NFC-Bereich beginnt.
	Offset int `json:"offset"`
	// ReadOnly: im Capability Container als schreibgeschützt kennzeichnen.
	// Fehlt das Feld, gilt true.
	ReadOnly *bool `json:"read_only"`
	// ExpectTID: nur schreiben, wenn genau dieser Tag aufliegt.
	ExpectTID string `json:"expect_tid"`
}

// writeURI beschreibt den einen aufgelegten Tag und liest zur Kontrolle
// zurück.
func (r *Reader) writeURI(c *atConn, p writeParams) (any, error) {
	if !strings.Contains(p.URI, "://") {
		return nil, device.Errf("bad_request", "uri fehlt oder ist keine Adresse")
	}
	if p.Offset < 0 || p.Offset%2 != 0 || p.Offset > 1024 {
		return nil, device.Errf("bad_request", "offset muss eine gerade Zahl zwischen 0 und 1024 sein")
	}
	readOnly := p.ReadOnly == nil || *p.ReadOnly
	tag, err := r.single(c)
	if err != nil {
		return nil, err
	}
	if p.ExpectTID != "" && !strings.EqualFold(p.ExpectTID, tag.TID) {
		return nil, device.Errf("tag_changed", "Am Reader liegt ein anderer Tag als erwartet.")
	}
	uri := strings.ReplaceAll(p.URI, "{tid}", tag.TID)
	data, err := EncodeType5URI(uri, readOnly)
	if err != nil {
		return nil, device.Errf("bad_request", err.Error())
	}
	err = r.withMask(c, tag.TID, func() error {
		// Einen vorhandenen Capability Container weiterverwenden: er nennt die
		// echte Größe des NFC-Bereichs (MLEN) und die Fähigkeiten des Chips.
		// Unser eigener kennt nur die Länge des Geschriebenen.
		if cc, err := r.readUSR(c, p.Offset, 4); err == nil {
			keepCC(data, cc)
		} else if fatal(err) {
			return err
		}
		for i := 0; i < len(data); i += writeChunk {
			part := data[i:min(i+writeChunk, len(data))]
			res, err := r.tagCommand(c, fmt.Sprintf("AT+WRT=USR,%d,%s", p.Offset+i, strings.ToUpper(hex.EncodeToString(part))), "+WRT: ")
			if err != nil {
				return err
			}
			if res.Status != "OK" {
				return device.Errf("write_failed", fmt.Sprintf("Schreiben bei Byte %d fehlgeschlagen: %s", p.Offset+i, res.Status))
			}
		}
		back, err := r.readUSRChunked(c, p.Offset, len(data))
		if err != nil {
			return err
		}
		if !slices.Equal(back, data) {
			return device.Errf("verify_failed", "Kontrolle nach dem Schreiben: Inhalt weicht ab.")
		}
		return nil
	})
	if err != nil {
		if isATError(err) {
			err = device.Errf("write_failed", "Reader meldet Fehler: "+err.Error())
		}
		return nil, err
	}
	r.bump(func(x *Stats) { x.Writes++ })
	r.log.Info("RFID-Tag beschrieben", "tid", tag.TID, "uri", uri)
	return map[string]any{
		"tid":       tag.TID,
		"epc":       tag.EPC,
		"uri":       uri,
		"bytes":     len(data),
		"offset":    p.Offset,
		"read_only": readOnly,
		"verified":  true,
	}, nil
}
