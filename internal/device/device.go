// Package device definiert das Treibermodell der Bridge: Geräte laufen in
// eigenen Goroutinen, melden Zustände und Messwerte über einen Bus und
// beantworten Kommandos.
package device

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// State ist der Verbindungszustand eines Geräts.
type State string

const (
	StateStarting State = "starting"
	StateOnline   State = "online"
	StateOffline  State = "offline" // konfiguriert, aber nicht erreichbar
	StateError    State = "error"   // erreichbar, liefert aber Unsinn / Timeouts
	StateDisabled State = "disabled"
)

// Status ist der Schnappschuss eines Geräts.
type Status struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Driver  string    `json:"driver"`
	Port    string    `json:"port,omitempty"`
	State   State     `json:"state"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
	Last    any       `json:"last,omitempty"`
	Stats   any       `json:"stats,omitempty"`
}

// Event geht an alle Abonnenten des Busses.
type Event struct {
	Name   string `json:"event"`
	Device string `json:"device,omitempty"`
	Data   any    `json:"data"`
}

// Device ist ein laufender Treiber.
type Device interface {
	ID() string
	Kind() string
	Status() Status
	// Run blockiert bis ctx endet und hält das Gerät in dieser Zeit verbunden.
	Run(ctx context.Context)
	// Handle führt ein Kommando aus, z.B. "read" oder "tare".
	Handle(ctx context.Context, cmd string, params json.RawMessage) (any, error)
}

// DemandTracker wird von Geräten implementiert, deren Abfragetakt von der
// Zahl der Zuhörer abhängt (Waage: schnell pollen nur, wenn jemand zusieht).
type DemandTracker interface {
	AddDemand(delta int)
}

// Error ist ein Fehler mit stabilem Code für den Client.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errf baut einen Error.
func Errf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Bus verteilt Events an Abonnenten. Langsame Abonnenten verlieren Events
// statt den Treiber zu blockieren.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBus erzeugt einen leeren Bus.
func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Subscribe liefert einen Kanal und eine Abmeldefunktion.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish verschickt ein Event, ohne zu blockieren.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// StatusHolder ist ein kleiner Helfer für Treiber: hält den Status und
// meldet Zustandswechsel als device.state auf dem Bus.
type StatusHolder struct {
	mu  sync.Mutex
	st  Status
	bus *Bus
}

// NewStatusHolder initialisiert den Status.
func NewStatusHolder(bus *Bus, id, kind, driver, port string) *StatusHolder {
	return &StatusHolder{bus: bus, st: Status{
		ID: id, Kind: kind, Driver: driver, Port: port,
		State: StateStarting, Since: time.Now(),
	}}
}

// Get liefert eine Kopie.
func (h *StatusHolder) Get() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st
}

// Set ändert Zustand und Meldung; nur echte Änderungen gehen auf den Bus.
func (h *StatusHolder) Set(state State, msg string) {
	h.mu.Lock()
	changed := h.st.State != state || h.st.Message != msg
	if h.st.State != state {
		h.st.Since = time.Now()
	}
	h.st.State, h.st.Message = state, msg
	snap := h.st
	h.mu.Unlock()
	if changed {
		h.bus.Publish(Event{Name: "device.state", Device: snap.ID, Data: stateData(snap)})
	}
}

// Update ändert Last/Stats/Port ohne Event.
func (h *StatusHolder) Update(fn func(*Status)) {
	h.mu.Lock()
	fn(&h.st)
	h.mu.Unlock()
}

func stateData(s Status) map[string]any {
	return map[string]any{"kind": s.Kind, "state": s.State, "message": s.Message}
}

// StateEvent baut das device.state-Event für einen Status, z.B. beim
// Entfernen eines Geräts.
func StateEvent(s Status) Event {
	return Event{Name: "device.state", Device: s.ID, Data: stateData(s)}
}
