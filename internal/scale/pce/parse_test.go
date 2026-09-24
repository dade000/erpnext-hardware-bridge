package pce

import "testing"

func TestParseReading(t *testing.T) {
	cases := []struct {
		raw  string
		kg   float64
		unit string
		ok   bool
	}{
		{"+     1.56kg", 1.56, "kg", true}, // real beobachtete Antwort
		{"-     0.12kg", -0.12, "kg", true},
		{"+    12.345 kg", 12.345, "kg", true},
		{"+  1560g", 1.56, "g", true},
		{"+  1,56kg", 1.56, "kg", true},
		{"0.00", 0, "kg", true},
		{"\x02+  2.00kg\x03", 2, "kg", true},
		{"+  2.00lb", 0.9072, "lb", true},
		{"", 0, "", false},
		{"ERR", 0, "", false},
		{"+  1.0kgs", 0, "", false},
		{"+ 1.0 2.0kg", 0, "", false},
	}
	for _, c := range cases {
		kg, unit, err := ParseReading(c.raw)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v, erwartet ok=%v", c.raw, err, c.ok)
			continue
		}
		if c.ok && (kg != c.kg || unit != c.unit) {
			t.Errorf("%q: %v %s, erwartet %v %s", c.raw, kg, unit, c.kg, c.unit)
		}
	}
}

func TestStability(t *testing.T) {
	s := stability{n: 3, tol: 0.005}
	seq := []struct {
		v      float64
		stable bool
	}{
		{1.00, false}, {1.00, false}, {1.00, true}, // drei gleiche
		{1.20, false},                 // Paket bewegt
		{1.203, false}, {1.201, true}, // innerhalb Toleranz
		{1.21, false},
	}
	for i, x := range seq {
		if got := s.add(x.v); got != x.stable {
			t.Fatalf("Schritt %d (%v): stable=%v, erwartet %v", i, x.v, got, x.stable)
		}
	}
	s.reset()
	if s.add(1.21) {
		t.Fatal("nach reset darf nicht sofort stabil sein")
	}
}
