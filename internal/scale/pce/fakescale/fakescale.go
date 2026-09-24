// Package fakescale simuliert eine PCE-PB-Waage hinter einem seriellen Port.
// Für Tests und für die Entwicklung ohne Hardware.
package fakescale

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Scale ist die simulierte Waage.
type Scale struct {
	mu       sync.Mutex
	weightKG float64
	silent   bool // antwortet nicht (ausgeschaltet)
	garbage  string
	tareKG   float64
	cmds     []string
}

// New erzeugt eine Waage mit Anfangsgewicht.
func New(kg float64) *Scale { return &Scale{weightKG: kg} }

// Set ändert das aufgelegte Gewicht.
func (s *Scale) Set(kg float64) { s.mu.Lock(); s.weightKG = kg; s.mu.Unlock() }

// Silent schaltet die Antworten ab.
func (s *Scale) Silent(v bool) { s.mu.Lock(); s.silent = v; s.mu.Unlock() }

// Garbage: nächste Antworten sind diese Zeile statt eines Gewichts.
func (s *Scale) Garbage(line string) { s.mu.Lock(); s.garbage = line; s.mu.Unlock() }

// Commands liefert die empfangenen Befehle.
func (s *Scale) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// Open liefert einen Port, der mit dieser Waage spricht.
func (s *Scale) Open() *Port { return &Port{s: s} }

// answer verarbeitet einen Befehl und liefert die Antwortbytes.
func (s *Scale) answer(cmd string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, cmd)
	switch cmd {
	case "ST":
		s.tareKG = s.weightKG
		return nil
	case "Sx":
		if s.silent {
			return nil
		}
		if s.garbage != "" {
			return []byte(s.garbage + "\r\n")
		}
		net := s.weightKG - s.tareKG
		sign := "+"
		if net < 0 {
			sign, net = "-", -net
		}
		return []byte(fmt.Sprintf("%s%10.2fkg\r\n", sign, net))
	}
	return nil
}

// Port implementiert serialport.Port.
type Port struct {
	s       *Scale
	mu      sync.Mutex
	in      bytes.Buffer // Befehle vom Treiber
	out     bytes.Buffer // Antworten an den Treiber
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
		line, err := p.in.ReadString('\n')
		if err != nil {
			p.in.WriteString(line) // unvollständig, zurücklegen
			break
		}
		p.out.Write(p.s.answer(string(bytes.TrimRight([]byte(line), "\r\n"))))
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
			n, _ := p.out.Read(b)
			p.mu.Unlock()
			return n, nil
		}
		p.mu.Unlock()
		if time.Now().After(deadline) {
			return 0, nil // wie go.bug.st/serial bei Timeout
		}
		time.Sleep(5 * time.Millisecond)
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
