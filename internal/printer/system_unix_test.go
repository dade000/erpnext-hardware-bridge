//go:build !windows

package printer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"erpnext-hardware-bridge/internal/config"
)

// fakeLP legt ein lp in den PATH, das Argumente und Daten mitschreibt.
func fakeLP(t *testing.T) (argsFile, dataFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, dataFile = filepath.Join(dir, "args"), filepath.Join(dir, "data")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done > " + argsFile + "\ncat > " + dataFile + "\n"
	if err := os.WriteFile(filepath.Join(dir, "lp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile, dataFile
}

func TestSystemRawBleibtRoh(t *testing.T) {
	argsFile, dataFile := fakeLP(t)
	s := newSystemSink(config.DeviceConfig{Queue: "Zebra", Media: "A4"})
	if got := s.Formats(); len(got) != 1 || got[0] != "zpl" {
		t.Fatalf("Formate %v, erwartet zpl", got)
	}
	if err := s.Write(context.Background(), Job{Title: "Label", Format: "zpl", Data: []byte("^XA^XZ")}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, argsFile); got != "-d\nZebra\n-t\nLabel\n-o\nraw\n" {
		t.Fatalf("lp-Argumente %q", got)
	}
	if got := readAll(t, dataFile); got != "^XA^XZ" {
		t.Fatalf("Daten %q", got)
	}
}

func TestSystemPDFDurchDenTreiber(t *testing.T) {
	argsFile, dataFile := fakeLP(t)
	s := newSystemSink(config.DeviceConfig{Queue: "OKI", Accept: "pdf", Media: "A4", PrintScaling: "fit"})
	if got := s.Formats(); len(got) != 1 || got[0] != "pdf" {
		t.Fatalf("Formate %v, erwartet pdf", got)
	}
	if err := s.Write(context.Background(), Job{Title: "Rechnung", Format: "pdf", Data: []byte("%PDF-1.4")}); err != nil {
		t.Fatal(err)
	}
	args := readAll(t, argsFile)
	if strings.Contains(args, "raw") {
		t.Fatalf("PDF darf nicht roh gehen: %q", args)
	}
	if args != "-d\nOKI\n-t\nRechnung\n-o\nmedia=A4\n-o\nprint-scaling=fit\n" {
		t.Fatalf("lp-Argumente %q", args)
	}
	if got := readAll(t, dataFile); got != "%PDF-1.4" {
		t.Fatalf("Daten %q", got)
	}
}
