// Package fakereader simuliert einen metratec DeskID UHF v2 hinter einem
// seriellen Port (AT-Protokoll). Für Tests und für die Entwicklung ohne
// Hardware. Nachgebaut ist nur, was der Treiber benutzt.
package fakereader

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tag ist ein simulierter Transponder.
type Tag struct {
	EPC string
	TID string
	USR []byte
	// FailWrite: Antwort statt OK auf Schreibbefehle (z.B. "ACCESS ERROR").
	FailWrite string
}

// Reader ist der simulierte Reader.
type Reader struct {
	mu     sync.Mutex
	echo   bool
	tags   []*Tag
	mask   string // TID-Maske, leer = alle
	silent bool
	power  int
	invs   []string
	cmds   []string
	hw     string
}

// New erzeugt einen Reader mit eingeschaltetem Echo (Werkszustand).
func New() *Reader {
	return &Reader{echo: true, invs: []string{"0", "1", "0", "0", "0", "ALL", "DUAL", "-100"}, hw: "DeskID_UHF_v2_E"}
}

// Hardware ändert die ATI-Kennung (für den Test "falscher Reader").
func (r *Reader) Hardware(hw string) { r.mu.Lock(); r.hw = hw; r.mu.Unlock() }

// Put legt Tags auf (ersetzt die bisherigen).
func (r *Reader) Put(tags ...*Tag) { r.mu.Lock(); r.tags = tags; r.mu.Unlock() }

// Silent schaltet die Antworten ab.
func (r *Reader) Silent(v bool) { r.mu.Lock(); r.silent = v; r.mu.Unlock() }

// Power liefert die zuletzt gesetzte Sendeleistung.
func (r *Reader) Power() int { r.mu.Lock(); defer r.mu.Unlock(); return r.power }

// Commands liefert die empfangenen Befehle.
func (r *Reader) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// Open liefert einen Port, der mit diesem Reader spricht.
func (r *Reader) Open() *Port { return &Port{r: r} }

func (r *Reader) visible() []*Tag {
	var out []*Tag
	for _, t := range r.tags {
		if r.mask == "" || strings.EqualFold(t.TID, r.mask) {
			out = append(out, t)
		}
	}
	return out
}

func (r *Reader) answer(cmd string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	if r.silent {
		return nil
	}
	var b strings.Builder
	if r.echo {
		b.WriteString(cmd + "\r\n")
	}
	ok := func(lines ...string) []byte {
		for _, l := range lines {
			b.WriteString(l + "\r\n")
		}
		b.WriteString("OK\r\n")
		return []byte(b.String())
	}
	fail := func(msg string) []byte {
		// Fehlgeschlagene Befehle werden nicht ge-echot.
		return []byte("+ERR: <" + msg + ">\r\nERROR\r\n")
	}
	name, arg, _ := strings.Cut(cmd, "=")
	switch name {
	case "AT":
		return ok()
	case "ATE0":
		r.echo = false
		return []byte("OK\r\n")
	case "ATE1":
		r.echo = true
		return ok()
	case "ATI":
		return ok("+SW: "+r.hw+" 0105", "+HW: "+r.hw+" 0100", "+SERIAL: 2026100900000001")
	case "AT+BINV":
		return fail("inventory not running")
	case "AT+INVS?":
		return ok("+INVS: " + strings.Join(r.invs, ","))
	case "AT+INVS":
		r.invs = strings.Split(arg, ",")
		return ok()
	case "AT+PWR":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 || n > 9 {
			return fail("power out of range")
		}
		r.power = n
		return ok()
	case "AT+MSK":
		if arg == "OFF" {
			r.mask = ""
			return ok()
		}
		f := strings.Split(arg, ",")
		if len(f) != 3 || f[0] != "TID" || f[1] != "0" {
			return fail("unsupported mask")
		}
		r.mask = f[2]
		return ok()
	case "AT+INV":
		tags := r.visible()
		if len(tags) == 0 {
			return ok("+INV: <NO TAGS FOUND>", "+INV: <ROUND FINISHED, ANT=1>")
		}
		var lines []string
		for _, t := range tags {
			line := "+INV: " + t.EPC
			if r.invs[2] == "1" {
				line += "," + t.TID
			}
			if r.invs[1] == "1" {
				line += ",-52"
			}
			lines = append(lines, line)
		}
		return ok(append(lines, "+INV: <ROUND FINISHED, ANT=1>")...)
	case "AT+READ":
		f := strings.Split(arg, ",")
		if len(f) < 3 || f[0] != "USR" {
			return fail("unsupported read")
		}
		start, _ := strconv.Atoi(f[1])
		n, _ := strconv.Atoi(f[2])
		tags := r.visible()
		if len(tags) == 0 {
			return ok("+READ: <NO TAGS FOUND>")
		}
		var lines []string
		for _, t := range tags {
			if start+n > len(t.USR) {
				lines = append(lines, "+READ: "+t.EPC+",MEMORY OVERRUN")
				continue
			}
			lines = append(lines, "+READ: "+t.EPC+",OK,"+strings.ToUpper(hex.EncodeToString(t.USR[start:start+n])))
		}
		return ok(lines...)
	case "AT+WRT":
		f := strings.Split(arg, ",")
		if len(f) < 3 || f[0] != "USR" {
			return fail("unsupported write")
		}
		start, _ := strconv.Atoi(f[1])
		data, err := hex.DecodeString(f[2])
		if err != nil {
			return fail("bad data")
		}
		tags := r.visible()
		if len(tags) == 0 {
			return ok("+WRT: <NO TAGS FOUND>")
		}
		var lines []string
		for _, t := range tags {
			switch {
			case t.FailWrite != "":
				lines = append(lines, "+WRT: "+t.EPC+","+t.FailWrite)
			case start+len(data) > len(t.USR):
				lines = append(lines, "+WRT: "+t.EPC+",MEMORY OVERRUN")
			default:
				copy(t.USR[start:], data)
				lines = append(lines, "+WRT: "+t.EPC+",OK")
			}
		}
		return ok(lines...)
	}
	return fail(fmt.Sprintf("unknown command %s", name))
}

// Port implementiert serialport.Port.
type Port struct {
	r       *Reader
	mu      sync.Mutex
	in      bytes.Buffer
	out     bytes.Buffer
	timeout time.Duration
	closed  bool
	broken  bool
}

// Break simuliert ein abgezogenes USB-Kabel.
func (p *Port) Break() { p.mu.Lock(); p.broken = true; p.mu.Unlock() }

func (p *Port) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.broken {
		return 0, errors.New("input/output error")
	}
	p.in.Write(b)
	for {
		line, err := p.in.ReadString('\r')
		if err != nil {
			p.in.WriteString(line)
			break
		}
		p.out.Write(p.r.answer(strings.TrimRight(line, "\r\n")))
	}
	return len(b), nil
}

func (p *Port) Read(b []byte) (int, error) {
	deadline := time.Now().Add(p.timeout)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		if p.broken {
			p.mu.Unlock()
			return 0, errors.New("input/output error")
		}
		if p.out.Len() > 0 {
			// Absichtlich kleine Häppchen: Zeilen kommen über mehrere Reads.
			n, _ := p.out.Read(b[:min(len(b), 7)])
			p.mu.Unlock()
			return n, nil
		}
		p.mu.Unlock()
		if time.Now().After(deadline) {
			return 0, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (p *Port) SetReadTimeout(d time.Duration) error { p.timeout = d; return nil }
func (p *Port) ResetInputBuffer() error {
	p.mu.Lock()
	p.out.Reset()
	p.mu.Unlock()
	return nil
}
func (p *Port) Close() error { p.mu.Lock(); p.closed = true; p.mu.Unlock(); return nil }
