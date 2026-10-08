//go:build !windows

package printer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"erpnext-hardware-bridge/internal/config"
)

// systemSink druckt über eine CUPS-Warteschlange des Stations-PCs. Rohdaten
// (ZPL, ESC/POS) gehen ungefiltert durch; ein PDF (accept: pdf) läuft durch
// die Filter und den Treiber der Warteschlange, so wie aus jedem Programm.
type systemSink struct {
	queue        string
	pdf          bool
	media        string
	printScaling string
}

func newSystemSink(cfg config.DeviceConfig) Sink {
	return &systemSink{queue: cfg.Queue, pdf: cfg.Accept == "pdf", media: cfg.Media, printScaling: cfg.PrintScaling}
}

func (s *systemSink) Formats() []string {
	if s.pdf {
		return []string{"pdf"}
	}
	return []string{"zpl"}
}

func (s *systemSink) Target() string { return "Warteschlange " + s.queue }

func (s *systemSink) Check(ctx context.Context) error {
	if s.queue == "" {
		return errors.New("keine Warteschlange konfiguriert")
	}
	out, err := exec.CommandContext(ctx, "lpstat", "-p", s.queue).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Warteschlange %q nicht gefunden (%s)", s.queue, firstLine(out, err))
	}
	if bytes.Contains(out, []byte("disabled")) {
		return fmt.Errorf("Warteschlange %q ist angehalten", s.queue)
	}
	return nil
}

func (s *systemSink) Write(ctx context.Context, job Job) error {
	cmd := exec.CommandContext(ctx, "lp", s.args(job)...)
	cmd.Stdin = bytes.NewReader(job.Data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(firstLine(out, err))
	}
	return nil
}

func (s *systemSink) args(job Job) []string {
	args := []string{"-d", s.queue, "-t", job.Title}
	if job.Format != "pdf" {
		// -o raw: CUPS reicht die Daten ungefiltert durch.
		return append(args, "-o", "raw")
	}
	if s.media != "" {
		args = append(args, "-o", "media="+s.media)
	}
	if s.printScaling != "" {
		args = append(args, "-o", "print-scaling="+s.printScaling)
	}
	return args
}

// ListQueues nennt die Druckwarteschlangen des Betriebssystems (für die
// Auswahl in der Oberfläche).
func ListQueues(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "lpstat", "-e").Output()
	if err != nil {
		return nil, fmt.Errorf("lpstat: %w", err)
	}
	var queues []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			queues = append(queues, line)
		}
	}
	return queues, nil
}

func firstLine(out []byte, err error) string {
	if line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); line != "" {
		return line
	}
	return err.Error()
}
