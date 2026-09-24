package device

import (
	"context"
	"encoding/json"

	"erpnext-hardware-bridge/internal/config"
)

// unsupported steht für ein konfiguriertes Gerät, dessen Treiber dieser Build
// nicht enthält (z.B. Kamera im Windows-Build). Es bleibt sichtbar, damit die
// Oberfläche und das Desk erklären können, warum nichts passiert.
type unsupported struct{ st *StatusHolder }

func newUnsupported(c config.DeviceConfig, bus *Bus) Device {
	h := NewStatusHolder(bus, c.ID, c.Kind, c.Driver, c.Port.String())
	return &unsupported{st: h}
}

func (u *unsupported) ID() string     { return u.st.Get().ID }
func (u *unsupported) Kind() string   { return u.st.Get().Kind }
func (u *unsupported) Status() Status { return u.st.Get() }

func (u *unsupported) Run(ctx context.Context) {
	s := u.st.Get()
	u.st.Set(StateError, "Treiber "+s.Kind+"/"+s.Driver+" ist in diesem Build nicht enthalten")
	<-ctx.Done()
}

func (u *unsupported) Handle(context.Context, string, json.RawMessage) (any, error) {
	return nil, Errf("unsupported", u.st.Get().Message)
}
