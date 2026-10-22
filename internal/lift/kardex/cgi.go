package kardex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tag ist eine Variable im Datenbaustein DB904 der C3000: Wort (DBW, 16 Bit)
// oder Doppelwort (DBD, 32 Bit) im Format x (hex), Bits 8 = Step7-String ab
// diesem Byte (DBB, Format n; Format s geht laut Handbuch nicht).
type Tag struct {
	Off  int
	Bits int
}

func w(off int) Tag { return Tag{off, 16} }
func d(off int) Tag { return Tag{off, 32} }
func b(off int) Tag { return Tag{off, 8} }

// Name ist die Schreibweise im CGI.
func (t Tag) Name() string {
	switch t.Bits {
	case 8:
		return "PDP,,DB904.DBB" + strconv.Itoa(t.Off) + ",n"
	case 16:
		return "PDP,,DB904.DBW" + strconv.Itoa(t.Off) + ",x"
	}
	return "PDP,,DB904.DBD" + strconv.Itoa(t.Off) + ",x"
}

func (t Tag) msb() uint32 {
	if t.Bits == 16 {
		return 0x8000
	}
	return 0x80000000
}

// Die Tags der Host-Schnittstelle (Handbuch HOST CGI Interface 1.14.2).
var (
	tagCMD = w(2) // Kommando, schreiben Host und Steuerung
	tagRET = d(4) // Rückgabe: oberes Wort Kommando, unteres Wort Status

	tagHI             = w(100)
	tagOpeningMsg     = d(102)
	tagMachineMsg     = d(106)
	tagVersion        = d(110)
	tagMachineBusy    = w(114)
	tagCurTray        = d(116) // VCM: CurShelfBase, Träger in der Bedienöffnung
	tagPosFlag        = w(120) // VCM: ShelfBasePosFlag
	tagLevelLower     = d(122)
	tagTrayLower      = d(126)
	tagPosLightFrom   = w(130)
	tagPosLightTo     = w(132)
	tagDepthLightFrom = w(134)
	tagDepthLightTo   = w(136)
	tagType           = w(138)
	tagNumOpenings    = w(140)
	tagNumTrays       = d(142)
	tagWidth          = d(146)
	tagDepth          = d(150)
	tagTrayUpper      = d(518)
	tagLevelUpper     = d(522)
	tagTrayLift       = d(526)
	tagLevelLift      = d(530)
	tagButton1        = w(534)
	tagButton2        = w(536)
	tagActiveHI       = w(618)
	tagOpeningBusy    = w(620)
	tagAutomaticMode  = w(634)
	tagSlidingDoor    = w(638)
)

// Kommandos.
const (
	cmdGetVersion     = 0x0001
	cmdGetDoorStatus  = 0x0002
	cmdDriveTray      = 0x0003
	cmdConfirm        = 0x0005
	cmdCancel         = 0x0006
	cmdGetMachineInfo = 0x0008
	cmdSetAlphaText   = 0x0009
)

// Globale Rückgabewerte (unteres Wort von RET), für alle Kommandos.
const (
	retNoCGI       = 0xFFF8
	retHostBusy    = 0xFFF9
	retFireAlarm   = 0xFFFA
	retNotHostMenu = 0xFFFB
	retUnknownCmd  = 0xFFFC
	retStandby     = 0xFFFD
	retNotPossible = 0xFFFE
	retService     = 0xFFFF
)

// Val ist ein Wert, der mit gesetztem Gültigkeitsbit (MSB) geschrieben wird.
type Val struct {
	Tag Tag
	V   uint32
	// Invalid schreibt 0 ohne Gültigkeitsbit: die Steuerung nimmt dann ihren
	// Standardwert. So bleibt kein alter Wert eines früheren Kommandos stehen.
	Invalid bool
	// S ist der Text bei String-Tags (schon fürs CGI kodiert).
	S string
}

func valid(t Tag, v uint32) Val { return Val{Tag: t, V: v} }
func unset(t Tag) Val           { return Val{Tag: t, Invalid: true} }
func text(t Tag, s string, n int) Val {
	return Val{Tag: t, S: cgiText(s, n)}
}

// encode liefert TAG=WERT für setValues.
func (v Val) encode() string {
	switch {
	case v.Tag.Bits == 8:
		return v.Tag.Name() + "=" + v.S
	case v.Invalid:
		return v.Tag.Name() + "=0"
	}
	return v.Tag.Name() + "=" + strconv.FormatUint(uint64(v.V|v.Tag.msb()), 16)
}

// Values sind gelesene OUT-Parameter.
type Values map[Tag]uint32

// Get liefert den Wert ohne Gültigkeitsbit und ob er gültig ist.
func (v Values) Get(t Tag) (uint32, bool) {
	x, ok := v[t]
	if !ok || x&t.msb() == 0 {
		return 0, false
	}
	return x &^ t.msb(), true
}

// StatusError ist ein Rückgabewert ungleich 0.
type StatusError struct {
	Cmd    uint16
	Status uint16
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Kommando 0x%04x: %s (Code %d / 0x%04x)", e.Cmd, StatusText(e.Cmd, e.Status), e.Status, e.Status)
}

// StatusText übersetzt einen Rückgabewert.
func StatusText(cmd, st uint16) string {
	switch st {
	case retNoCGI:
		return "keine CGI-Schnittstelle (Parameter 400.1 steht auf UDP)"
	case retHostBusy:
		return "Host-Funktion der Steuerung ist beschäftigt"
	case retFireAlarm:
		return "Brandalarm"
	case retNotHostMenu:
		return "Lift ist nicht im Host-Menü"
	case retUnknownCmd:
		return "unbekanntes Kommando"
	case retStandby:
		return "Lift ist im Standby"
	case retNotPossible:
		return "Funktion gerade nicht möglich"
	case retService:
		return "Lift ist im Service-Betrieb"
	case 1:
		return "Bedienöffnung (HI) ungültig"
	}
	if cmd == cmdDriveTray {
		switch st {
		case 10:
			return "Träger nicht in der Tablar-Tabelle"
		case 11:
			return "Träger in der Tabelle, aber nicht in der Maschine"
		case 12:
			return "Träger in einer anderen Bedienöffnung"
		case 18:
			return "Träger schon von einer anderen Bedienöffnung angefordert"
		case 25:
			return "letzter Host-Befehl läuft noch"
		case 26:
			return "keine gültige Trägernummer"
		case 27:
			return "Bedienöffnung nicht initialisiert"
		case 28:
			return "Roboterzugriff aktiv"
		}
	}
	return "Fehler"
}

// ErrBusy: CMD wurde nicht rechtzeitig 0, die Steuerung arbeitet noch an
// einem Kommando (von uns oder einem anderen Host).
var ErrBusy = errors.New("Steuerung bearbeitet noch ein Kommando")

// Client spricht die CGI-Schnittstelle der C3000 (HTTP GET auf Port 81).
// Die Steuerung kann nur einen Host bedienen; Kommandos laufen deshalb
// nacheinander.
type Client struct {
	base string
	http *http.Client
	poll time.Duration
	// cmdTimeout: so lange darf die Steuerung brauchen, bis sie CMD auf 0
	// zurücksetzt. Das ist die Annahme des Kommandos, nicht die Fahrt.
	cmdTimeout time.Duration

	// debug bekommt jede Anfrage samt Antwort (Log-Stufe debug), für die
	// Inbetriebnahme am echten Lift.
	debug func(msg string, args ...any)

	exec  sync.Mutex // ein Kommando zur Zeit
	mu    sync.Mutex
	lists map[string]string // Tag-Liste → Listenname (OrderValues)
	seq   int
}

// NewClient: addr ist host:port.
func NewClient(addr string) *Client {
	return &Client{
		base:       "http://" + addr,
		http:       &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}},
		poll:       100 * time.Millisecond,
		cmdTimeout: 10 * time.Second,
		lists:      map[string]string{},
	}
}

func (c *Client) get(ctx context.Context, path, query string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	// Die Steuerung erwartet die Tags roh (Kommas, + als Trenner); nichts
	// darf hier noch einmal kodiert werden.
	req.URL.RawQuery = query
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if c.debug != nil {
		c.debug("CGI", "path", path, "query", query, "status", resp.StatusCode, "body", short(string(b)))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return string(b), nil
}

func (c *Client) setRaw(ctx context.Context, query string) error {
	body, err := c.get(ctx, "/cgi-bin/setValues.exe", query)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "Done") {
		return fmt.Errorf("setValues: unerwartete Antwort %q", short(body))
	}
	return nil
}

// cmdVal schreibt CMD ohne Gültigkeitsbit (so im Handbuch, Beispiele S. 57 ff.).
func cmdVal(cmd uint16) string { return tagCMD.Name() + "=" + strconv.FormatUint(uint64(cmd), 16) }

// read liest Tags über eine Liste (OrderValues.exe + ReadFile.exe). Die
// Liste wird einmal angelegt; nach einem Neustart der Steuerung ist sie weg
// und wird neu bestellt.
func (c *Client) read(ctx context.Context, tags []Tag) (Values, error) {
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name()
	}
	key := strings.Join(names, "+")
	c.mu.Lock()
	list, ok := c.lists[key]
	c.mu.Unlock()
	var lastBody string
	var lastMissing int
	for attempt := 0; attempt < 2; attempt++ {
		if !ok {
			c.mu.Lock()
			c.seq++
			list = "brg" + strconv.Itoa(c.seq)
			c.mu.Unlock()
			size := 64
			for _, n := range names {
				size += len(n) + 16
			}
			body, err := c.get(ctx, "/cgi-bin/OrderValues.exe", list+"+dummy+"+strconv.Itoa(size)+"+"+key)
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(strings.TrimSpace(body), "Done") {
				return nil, fmt.Errorf("OrderValues: unerwartete Antwort %q", short(body))
			}
			c.mu.Lock()
			c.lists[key] = list
			c.mu.Unlock()
		}
		body, err := c.get(ctx, "/cgi-bin/ReadFile.exe", list)
		if err != nil {
			return nil, err
		}
		vals, missing := parseList(body, tags)
		if missing == 0 {
			return vals, nil
		}
		lastBody, lastMissing = body, missing
		// Liste unbekannt (Steuerung neu gestartet): neu bestellen.
		c.mu.Lock()
		delete(c.lists, key)
		c.mu.Unlock()
		ok = false
	}
	return nil, fmt.Errorf("ReadFile: Steuerung liefert %d von %d bestellten Werten nicht, Antwort %q", lastMissing, len(tags), short(lastBody))
}

func parseList(body string, tags []Tag) (Values, int) {
	byName := map[string]Tag{}
	for _, t := range tags {
		byName[strings.ToLower(t.Name())] = t
	}
	vals := Values{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		name, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		t, ok := byName[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 16, 32)
		if err != nil {
			continue
		}
		vals[t] = uint32(n)
	}
	return vals, len(tags) - len(vals)
}

// Exec führt ein Kommando nach dem Ablauf im Handbuch aus: warten bis CMD 0
// ist, IN-Parameter und zuletzt CMD schreiben, warten bis die Steuerung CMD
// wieder auf 0 setzt, dann RET und die OUT-Parameter lesen.
func (c *Client) Exec(ctx context.Context, cmd uint16, in []Val, out []Tag) (Values, error) {
	c.exec.Lock()
	defer c.exec.Unlock()
	if err := c.waitIdle(ctx, nil); err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(in)+1)
	for _, v := range in {
		parts = append(parts, v.encode())
	}
	// CMD zuletzt: mit der steigenden Flanke liest die Steuerung die IN-Parameter.
	parts = append(parts, cmdVal(cmd))
	if err := c.setRaw(ctx, strings.Join(parts, "&")); err != nil {
		return nil, err
	}
	tags := append([]Tag{tagCMD, tagRET}, out...)
	var vals Values
	if err := c.waitIdle(ctx, func(v Values) { vals = v }, tags...); err != nil {
		return nil, err
	}
	ret := vals[tagRET]
	if hi := uint16(ret >> 16); hi != cmd {
		return nil, fmt.Errorf("Rückgabe 0x%08x gehört zu Kommando 0x%04x statt 0x%04x (anderer Host an der Steuerung?)", ret, hi, cmd)
	}
	if st := uint16(ret); st != 0 {
		return vals, &StatusError{Cmd: cmd, Status: st}
	}
	return vals, nil
}

// waitIdle pollt, bis CMD 0 ist. extra werden in derselben Abfrage gelesen
// und an got übergeben.
func (c *Client) waitIdle(ctx context.Context, got func(Values), extra ...Tag) error {
	tags := extra
	if len(tags) == 0 {
		tags = []Tag{tagCMD}
	}
	deadline := time.Now().Add(c.cmdTimeout)
	for {
		v, err := c.read(ctx, tags)
		if err != nil {
			return err
		}
		if v[tagCMD] == 0 {
			if got != nil {
				got(v)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll):
		}
	}
}

// cgiText kodiert einen Text für das CGI: höchstens n Zeichen, nur ASCII,
// reservierte Zeichen als %XX (Handbuch »Invalid characters«).
func cgiText(s string, n int) string {
	r := strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss", "€", "EUR")
	s = r.Replace(s)
	var b strings.Builder
	count := 0
	for _, ch := range s {
		if count == n {
			break
		}
		count++
		switch {
		case ch < 0x20 || ch > 0x7e:
			b.WriteByte('?')
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', strings.ContainsRune("-_.!~*'()", ch):
			b.WriteRune(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func short(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " | ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
