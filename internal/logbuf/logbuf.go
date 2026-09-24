// Package logbuf richtet das Logging ein: Textausgabe auf stdout, in eine
// rotierende Datei und in einen Ringpuffer für die Statusseite.
package logbuf

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Ring hält die letzten n Zeilen.
type Ring struct {
	mu    sync.Mutex
	lines []string
	n     int
	part  strings.Builder
}

// NewRing erzeugt einen Ringpuffer.
func NewRing(n int) *Ring { return &Ring{n: n} }

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			r.lines = append(r.lines, r.part.String())
			r.part.Reset()
			if len(r.lines) > r.n {
				r.lines = r.lines[len(r.lines)-r.n:]
			}
			continue
		}
		r.part.WriteByte(b)
	}
	return len(p), nil
}

// Lines liefert eine Kopie, älteste zuerst.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// RotatingFile schreibt in path und benennt die Datei bei maxBytes nach
// path.1 um. Eine Generation reicht für die Fehlersuche vor Ort.
type RotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// OpenRotating öffnet die Logdatei zum Anhängen.
func OpenRotating(path string, maxBytes int64) (*RotatingFile, error) {
	rf := &RotatingFile{path: path, max: maxBytes}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (rf *RotatingFile) open() error {
	f, err := os.OpenFile(rf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	rf.f, rf.size = f, 0
	if st != nil {
		rf.size = st.Size()
	}
	return nil
}

func (rf *RotatingFile) Write(p []byte) (int, error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.size+int64(len(p)) > rf.max {
		rf.f.Close()
		_ = os.Rename(rf.path, rf.path+".1")
		if err := rf.open(); err != nil {
			return 0, err
		}
	}
	n, err := rf.f.Write(p)
	rf.size += int64(n)
	return n, err
}

// Close schließt die Datei.
func (rf *RotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.f.Close()
}

// Setup baut den Logger. level lässt sich zur Laufzeit ändern.
func Setup(ring *Ring, extra ...io.Writer) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	w := io.MultiWriter(append([]io.Writer{os.Stdout, ring}, extra...)...)
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(h), lv
}

// ParseLevel übersetzt log_level aus der Config.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
