package pce

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Die PCE-PB N antwortet auf "Sx" mit einer Zeile wie "+     1.56kg". Das
// Vorzeichen steht vorne, getrennt durch Leerzeichen; die Einheit folgt ohne
// Abstand. Andere Einheiten entstehen, wenn jemand die Waage umstellt.
var readingRe = regexp.MustCompile(`^\s*([-+])?\s*(\d+(?:[.,]\d+)?)\s*([a-zA-Z]*)\s*$`)

var unitToKG = map[string]float64{
	"":   1, // ohne Einheit: kg, wie in der bisherigen Flask-App angenommen
	"kg": 1,
	"g":  0.001,
	"lb": 0.45359237,
	"oz": 0.028349523125,
}

// ParseReading wandelt eine Antwortzeile in Kilogramm um.
func ParseReading(raw string) (kg float64, unit string, err error) {
	s := strings.TrimSpace(strings.Trim(raw, "\x00\x02\x03"))
	m := readingRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", fmt.Errorf("keine Gewichtsangabe in %q", raw)
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", "."), 64)
	if err != nil {
		return 0, "", fmt.Errorf("Zahl in %q: %w", raw, err)
	}
	unit = strings.ToLower(m[3])
	f, ok := unitToKG[unit]
	if !ok {
		return 0, "", fmt.Errorf("unbekannte Einheit %q in %q", unit, raw)
	}
	if m[1] == "-" {
		v = -v
	}
	if unit == "" {
		unit = "kg"
	}
	return round(v*f, 4), unit, nil
}

func round(v float64, digits int) float64 {
	p := 1.0
	for i := 0; i < digits; i++ {
		p *= 10
	}
	if v < 0 {
		return -float64(int64(-v*p+0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// stability erkennt ein ruhendes Gewicht: die letzten n Messungen liegen
// innerhalb der Toleranz. Die Waage liefert im Sx-Format keinen eigenen
// Stabil-Marker.
type stability struct {
	n   int
	tol float64
	buf []float64
}

func (s *stability) add(v float64) bool {
	s.buf = append(s.buf, v)
	if len(s.buf) > s.n {
		s.buf = s.buf[len(s.buf)-s.n:]
	}
	if len(s.buf) < s.n {
		return false
	}
	lo, hi := s.buf[0], s.buf[0]
	for _, x := range s.buf[1:] {
		lo, hi = min(lo, x), max(hi, x)
	}
	return hi-lo <= s.tol+1e-9
}

func (s *stability) reset() { s.buf = s.buf[:0] }
