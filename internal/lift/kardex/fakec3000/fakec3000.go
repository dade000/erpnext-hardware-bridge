// Package fakec3000 baut die HOST-CGI-Schnittstelle einer Kardex-C3000 mit
// einem vertikalen Umlaufregal (Megamat, VCM) nach: setValues.exe,
// OrderValues.exe und ReadFile.exe auf DB904, CMD/RET-Handshake und ein
// Umlauf, der Träger für Träger zur Bedienöffnung fährt.
//
// Für Tests und den Simulator-Treiber der Bridge. Nachgebaut ist nur, was
// die Bridge benutzt; das Verhalten folgt dem Handbuch, nicht einem echten
// Gerät.
package fakec3000

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	msbW = 0x8000
	msbD = 0x80000000
)

// Machine ist die nachgebaute Steuerung.
type Machine struct {
	// Step: Fahrzeit je Träger.
	Step time.Duration
	// CmdDelay: so lange braucht die Steuerung, bis sie CMD auf 0 setzt.
	CmdDelay time.Duration

	mu       sync.Mutex
	trays    int
	cur      int // Träger in der Bedienöffnung, 0 = unterwegs
	pos      int // Stellung des Umlaufs (Träger vor der Öffnung)
	target   int
	hostMenu bool
	mem      map[string]uint32
	strs     map[int]string
	lists    map[string][]string
	drives   []int
	requests int
	stop     chan struct{}
}

// New baut eine Maschine mit trays Trägern; Träger 1 steht in der Öffnung.
func New(trays int) *Machine {
	return &Machine{
		Step: 200 * time.Millisecond, CmdDelay: 20 * time.Millisecond,
		trays: trays, cur: 1, pos: 1, hostMenu: true,
		mem: map[string]uint32{}, strs: map[int]string{}, lists: map[string][]string{},
		stop: make(chan struct{}),
	}
}

// Close hält laufende Fahrten an.
func (m *Machine) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

// SetHostMenu: false = Lift steht nicht im Host-Menü (Kommandos → 0xFFFB).
func (m *Machine) SetHostMenu(on bool) {
	m.mu.Lock()
	m.hostMenu = on
	m.mu.Unlock()
}

// Restart vergisst die bestellten Listen, wie nach einem Neustart.
func (m *Machine) Restart() {
	m.mu.Lock()
	m.lists = map[string][]string{}
	m.mu.Unlock()
}

// Current liefert den Träger in der Öffnung (0 = unterwegs).
func (m *Machine) Current() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// Drives liefert die angeforderten Träger in Reihenfolge.
func (m *Machine) Drives() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.drives...)
}

// Text liefert einen geschriebenen String ab Byte off (z.B. 154 = AlphaText1).
func (m *Machine) Text(off int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.strs[off]
}

// Requests zählt HTTP-Anfragen.
func (m *Machine) Requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

var tagRe = regexp.MustCompile(`^PDP,,DB904\.DB([BWD])(\d+),([a-zA-Z])$`)

func key(kind string, off int) string { return kind + strconv.Itoa(off) }

// ServeHTTP beantwortet die CGI-Kommandos.
func (m *Machine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	m.mu.Unlock()
	q := r.URL.RawQuery
	w.Header().Set("Content-Type", "text/plain")
	switch strings.ToLower(r.URL.Path) {
	case "/cgi-bin/setvalues.exe":
		if err := m.setValues(q); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "Done\n")
	case "/cgi-bin/ordervalues.exe":
		parts := strings.Split(q, "+")
		if len(parts) < 4 || parts[1] != "dummy" {
			http.Error(w, "bad list", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.lists[parts[0]] = parts[3:]
		m.mu.Unlock()
		fmt.Fprint(w, "Done.\n")
	case "/cgi-bin/readfile.exe":
		m.mu.Lock()
		tags, ok := m.lists[q]
		var b strings.Builder
		if ok {
			for _, t := range tags {
				mm := tagRe.FindStringSubmatch(t)
				if mm == nil {
					continue
				}
				off, _ := strconv.Atoi(mm[2])
				fmt.Fprintf(&b, "%s=%x\n", t, m.mem[key(mm[1], off)])
			}
		}
		m.mu.Unlock()
		// Unbekannte Liste: leere Antwort (wie nach einem Neustart).
		fmt.Fprint(w, b.String())
	default:
		http.NotFound(w, r)
	}
}

func (m *Machine) setValues(q string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, part := range strings.Split(q, "&") {
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			return fmt.Errorf("kein = in %q", part)
		}
		mm := tagRe.FindStringSubmatch(name)
		if mm == nil {
			return fmt.Errorf("Tag %q unbekannt", name)
		}
		off, _ := strconv.Atoi(mm[2])
		if mm[1] == "B" {
			if mm[3] != "n" {
				return fmt.Errorf("Strings nur im Format n")
			}
			s, err := url.PathUnescape(val)
			if err != nil {
				return err
			}
			m.strs[off] = s
			continue
		}
		n, err := strconv.ParseUint(val, 16, 32)
		if err != nil {
			return fmt.Errorf("Wert %q: %v", val, err)
		}
		m.mem[key(mm[1], off)] = uint32(n)
		if mm[1] == "W" && off == 2 && n != 0 {
			cmd := uint16(n)
			go func() {
				time.Sleep(m.CmdDelay)
				m.execute(cmd)
			}()
		}
	}
	return nil
}

func (m *Machine) w(off int, v uint32) { m.mem[key("W", off)] = msbW | v }
func (m *Machine) d(off int, v uint32) { m.mem[key("D", off)] = msbD | v }

func (m *Machine) in(kind string, off int) (uint32, bool) {
	v := m.mem[key(kind, off)]
	msb := uint32(msbD)
	if kind == "W" {
		msb = msbW
	}
	return v &^ msb, v&msb != 0
}

func (m *Machine) execute(cmd uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.run(cmd)
	m.mem[key("D", 4)] = uint32(cmd)<<16 | uint32(st)
	m.mem[key("W", 2)] = 0
}

func (m *Machine) run(cmd uint16) uint16 {
	if cmd > 0x17 {
		return 0xFFFC
	}
	if !m.hostMenu {
		return 0xFFFB
	}
	if cmd != 0x01 && cmd != 0x08 {
		if hi, ok := m.in("W", 100); !ok || hi != 1 {
			return 1
		}
	}
	switch cmd {
	case 0x01:
		m.d(110, 0x010E02)
	case 0x08:
		m.w(138, 5)
		m.w(140, 1)
		m.d(142, uint32(m.trays))
		m.d(146, 2450)
		m.d(150, 540)
	case 0x02:
		busy := uint32(0)
		if m.cur == 0 {
			busy = 1
		}
		m.w(114, busy)
		m.d(116, uint32(m.cur))
		m.w(120, 1-busy)
		m.d(102, 0)
		m.d(106, 0)
		m.w(618, 1)
		m.w(620, busy)
		m.w(634, 1)
	case 0x03:
		tray, ok := m.in("D", 126)
		if !ok || tray == 0 {
			return 26
		}
		if int(tray) > m.trays {
			return 10
		}
		if m.cur == 0 {
			return 25
		}
		m.drives = append(m.drives, int(tray))
		if int(tray) != m.cur {
			m.target = int(tray)
			m.cur = 0
			go m.rotate()
		}
	case 0x05, 0x06, 0x09:
	default:
		return 0xFFFE
	}
	return 0
}

// rotate fährt den Umlauf auf kürzestem Weg zum Ziel.
func (m *Machine) rotate() {
	for {
		select {
		case <-m.stop:
			return
		case <-time.After(m.Step):
		}
		m.mu.Lock()
		if m.pos == m.target {
			m.cur = m.pos
			m.mu.Unlock()
			return
		}
		fwd := (m.target - m.pos + m.trays) % m.trays
		if fwd <= m.trays/2 {
			m.pos = m.pos%m.trays + 1
		} else {
			m.pos = (m.pos+m.trays-2)%m.trays + 1
		}
		m.mu.Unlock()
	}
}
