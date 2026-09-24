// Package serialport öffnet serielle Geräte plattformneutral, entweder über
// den Pfad (/dev/waage, COM5) oder über USB-VID/PID/Seriennummer.
package serialport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"erpnext-hardware-bridge/internal/config"
)

// Port ist der Teil von serial.Port, den die Treiber brauchen. Tests setzen
// hier eine Attrappe ein.
type Port interface {
	io.ReadWriteCloser
	SetReadTimeout(time.Duration) error
	ResetInputBuffer() error
}

// Info beschreibt einen erkannten Port für die Oberfläche.
type Info struct {
	Name    string `json:"name"`
	IsUSB   bool   `json:"is_usb"`
	VID     string `json:"vid,omitempty"`
	PID     string `json:"pid,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Product string `json:"product,omitempty"`
	// Aliases sind Symlinks, die auf diesen Port zeigen (z.B. /dev/waage).
	Aliases []string `json:"aliases,omitempty"`
}

// ErrNotFound: kein Gerät passt auf die Angabe (Kabel ab, falscher Port).
var ErrNotFound = errors.New("serieller Port nicht gefunden")

// List liefert alle erkannten Ports.
func List() ([]Info, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	aliases := linuxAliases()
	out := make([]Info, 0, len(details))
	for _, d := range details {
		out = append(out, Info{
			Name:    d.Name,
			IsUSB:   d.IsUSB,
			VID:     strings.ToLower(d.VID),
			PID:     strings.ToLower(d.PID),
			Serial:  d.SerialNumber,
			Product: d.Product,
			Aliases: aliases[d.Name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Resolve übersetzt eine PortSpec in einen öffnbaren Pfad.
func Resolve(spec config.PortSpec) (string, error) {
	if spec.Path != "" {
		return spec.Path, nil
	}
	if spec.Match == nil {
		return "", errors.New("kein Port angegeben")
	}
	ports, err := List()
	if err != nil {
		return "", fmt.Errorf("Ports auflisten: %w", err)
	}
	m := spec.Match
	var hits []string
	for _, p := range ports {
		if !p.IsUSB || !strings.EqualFold(p.VID, m.VID) || !strings.EqualFold(p.PID, m.PID) {
			continue
		}
		if m.Serial != "" && p.Serial != m.Serial {
			continue
		}
		hits = append(hits, p.Name)
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNotFound, spec)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("%s passt auf mehrere Ports (%s) – Seriennummer ergänzen", spec, strings.Join(hits, ", "))
	}
}

// Open öffnet den Port mit 8N1 und der angegebenen Baudrate.
func Open(spec config.PortSpec, baud int) (Port, string, error) {
	path, err := Resolve(spec)
	if err != nil {
		return nil, "", err
	}
	p, err := serial.Open(path, &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		var pe *serial.PortError
		if errors.As(err, &pe) && pe.Code() == serial.PortNotFound {
			return nil, path, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil, path, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, path, fmt.Errorf("%s öffnen: %w", path, err)
	}
	return p, path, nil
}

// linuxAliases findet udev-Symlinks in /dev (z.B. /dev/waage -> ttyUSB0),
// damit die Oberfläche den gewohnten Namen anzeigen kann.
func linuxAliases() map[string][]string {
	out := map[string][]string{}
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join("/dev", e.Name())
		target, err := filepath.EvalSymlinks(link)
		if err != nil || !strings.HasPrefix(target, "/dev/tty") {
			continue
		}
		out[target] = append(out[target], link)
	}
	// Auch die stabilen Namen aus /dev/serial/by-id anbieten.
	byID, _ := filepath.Glob("/dev/serial/by-id/*")
	for _, link := range byID {
		if target, err := filepath.EvalSymlinks(link); err == nil {
			out[target] = append(out[target], link)
		}
	}
	return out
}
