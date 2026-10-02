//go:build !windows

package printer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// systemSink druckt roh über eine CUPS-Warteschlange des Stations-PCs.
type systemSink struct {
	rawOnly
	queue string
}

func newSystemSink(queue string) Sink { return &systemSink{queue: queue} }

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
	title, data := job.Title, job.Data
	// -o raw: CUPS reicht die Daten ungefiltert durch.
	cmd := exec.CommandContext(ctx, "lp", "-d", s.queue, "-o", "raw", "-t", title)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(firstLine(out, err))
	}
	return nil
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
