package device

import (
	"context"
	"log/slog"
	"reflect"
	"sort"
	"sync"

	"erpnext-hardware-bridge/internal/config"
)

// Factory baut aus einer Gerätekonfiguration einen Treiber. Liefert sie
// nil, ist der Treiber in diesem Build nicht enthalten.
type Factory func(cfg config.DeviceConfig, bus *Bus, log *slog.Logger) Device

type running struct {
	cfg    config.DeviceConfig
	dev    Device
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager startet, stoppt und tauscht Geräte gemäß Konfiguration.
type Manager struct {
	bus       *Bus
	log       *slog.Logger
	factories map[string]Factory // key: kind/driver

	mu      sync.RWMutex
	devices map[string]*running
	order   []string
}

// NewManager erzeugt einen Manager ohne Geräte.
func NewManager(bus *Bus, log *slog.Logger) *Manager {
	return &Manager{bus: bus, log: log, factories: map[string]Factory{}, devices: map[string]*running{}}
}

// Register macht einen Treiber bekannt.
func (m *Manager) Register(kind, driver string, f Factory) {
	m.factories[kind+"/"+driver] = f
}

// Drivers listet die registrierten Treiber je Klasse (für die Oberfläche).
func (m *Manager) Drivers() map[string][]string {
	out := map[string][]string{}
	for k := range m.factories {
		for i := 0; i < len(k); i++ {
			if k[i] == '/' {
				out[k[:i]] = append(out[k[:i]], k[i+1:])
				break
			}
		}
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

// Apply gleicht die laufenden Geräte mit der Konfiguration ab: entfernte und
// geänderte werden gestoppt, neue und geänderte gestartet, unveränderte
// laufen ungestört weiter.
func (m *Manager) Apply(cfgs []config.DeviceConfig) {
	m.mu.Lock()
	want := map[string]config.DeviceConfig{}
	order := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		want[c.ID] = c
		order = append(order, c.ID)
	}
	var stop []*running
	for id, r := range m.devices {
		if c, ok := want[id]; !ok || !reflect.DeepEqual(c, r.cfg) {
			stop = append(stop, r)
			delete(m.devices, id)
		}
	}
	m.order = order
	m.mu.Unlock()

	for _, r := range stop {
		r.cancel()
		<-r.done
		st := r.dev.Status()
		if _, still := want[st.ID]; !still {
			st.State, st.Message = StateDisabled, "aus der Konfiguration entfernt"
			m.bus.Publish(StateEvent(st))
		}
		m.log.Info("Gerät gestoppt", "device", st.ID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range cfgs {
		if _, ok := m.devices[c.ID]; ok {
			continue
		}
		var dev Device
		if f := m.factories[c.Kind+"/"+c.Driver]; f != nil {
			dev = f(c, m.bus, m.log.With("device", c.ID))
		}
		if dev == nil {
			dev = newUnsupported(c, m.bus)
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &running{cfg: c, dev: dev, cancel: cancel, done: make(chan struct{})}
		m.devices[c.ID] = r
		go func() {
			defer close(r.done)
			dev.Run(ctx)
		}()
		m.log.Info("Gerät gestartet", "device", c.ID, "kind", c.Kind, "driver", c.Driver, "port", c.Port.String())
	}
}

// Stop beendet alle Geräte (beim Herunterfahren).
func (m *Manager) Stop() { m.Apply(nil) }

// Get liefert ein Gerät über die ID.
func (m *Manager) Get(id string) Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r := m.devices[id]; r != nil {
		return r.dev
	}
	return nil
}

// FirstOfKind liefert das erste konfigurierte Gerät einer Klasse. So muss
// das Desk die Geräte-ID nicht kennen.
func (m *Manager) FirstOfKind(kind string) Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		if r := m.devices[id]; r != nil && r.dev.Kind() == kind {
			return r.dev
		}
	}
	return nil
}

// Lookup: explizite ID oder, wenn leer, das erste Gerät der Klasse.
func (m *Manager) Lookup(kind, id string) (Device, *Error) {
	var d Device
	if id != "" {
		d = m.Get(id)
	} else {
		d = m.FirstOfKind(kind)
	}
	if d == nil || d.Kind() != kind {
		if id == "" {
			return nil, Errf("device_missing", "An dieser Station ist kein Gerät der Klasse "+kind+" eingerichtet.")
		}
		return nil, Errf("device_missing", "Gerät "+id+" ist nicht eingerichtet.")
	}
	return d, nil
}

// Statuses liefert alle Geräte in Konfigurationsreihenfolge.
func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.order))
	for _, id := range m.order {
		if r := m.devices[id]; r != nil {
			out = append(out, r.dev.Status())
		}
	}
	return out
}
