package printer

import (
	"context"
	"errors"
	"os"
)

// fileSink schreibt in eine Gerätedatei, z.B. /dev/usb/lp0 (USB-Drucker
// unter Linux ohne Druckwarteschlange).
type fileSink struct{ path string }

func (s *fileSink) Target() string { return s.path }

func (s *fileSink) Check(context.Context) error {
	if s.path == "" {
		return errors.New("kein Pfad konfiguriert")
	}
	// Nur prüfen, ob es das Gerät gibt: Öffnen kann bei einem USB-Drucker
	// blockieren, solange er beschäftigt ist.
	_, err := os.Stat(s.path)
	return err
}

func (s *fileSink) Write(ctx context.Context, _ string, data []byte) error {
	f, err := os.OpenFile(s.path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, werr := f.Write(data)
		done <- werr
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
