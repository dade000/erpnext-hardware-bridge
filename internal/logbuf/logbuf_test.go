package logbuf

import (
	"errors"
	"strings"
	"testing"
)

type broken struct{}

func (broken) Write([]byte) (int, error) { return 0, errors.New("keine Konsole") }

// Als Windows-Dienst scheitert stdout. Die übrigen Ziele müssen die Zeile
// trotzdem bekommen.
func TestFailingWriterDoesNotSwallowLines(t *testing.T) {
	ring := NewRing(10)
	var file strings.Builder
	w := allWriters{broken{}, ring, &file}
	if _, err := w.Write([]byte("Kasse mit Terminal verbunden\n")); err != nil {
		t.Fatal(err)
	}
	if got := ring.Lines(); len(got) != 1 || got[0] != "Kasse mit Terminal verbunden" {
		t.Fatalf("Ring: %q", got)
	}
	if file.String() != "Kasse mit Terminal verbunden\n" {
		t.Fatalf("Datei: %q", file.String())
	}
}
