package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/logbuf"
)

// Das Desk fordert Träger 21 an; die Antwort kommt erst, wenn er in der
// Bedienöffnung steht, dazu lift.arrived an den Abonnenten.
func TestWebSocketLiftFetch(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Station: "lager", ListenPort: port, AllowedOrigins: []string{origin},
		Devices: []config.DeviceConfig{{ID: "megamat", Kind: "lift", Driver: "simulator"}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ring := logbuf.NewRing(100)
	_, lv := logbuf.Setup(ring)
	a := New(filepath.Join(t.TempDir(), "bridge.yaml"), cfg, false, slog.New(slog.NewTextHandler(io.Discard, nil)), lv, ring)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a.Shutdown(ctx)
	})
	dev := a.Manager().Get("megamat")
	deadline := time.Now().Add(3 * time.Second)
	for dev.Status().State != "online" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cl, _, err := dial(t, "127.0.0.1", port, origin)
	if err != nil {
		t.Fatal(err)
	}
	cl.hello()
	if m := cl.call("lift.subscribe", map[string]any{}); !m.OK {
		t.Fatalf("subscribe: %+v", m.Error)
	}
	// Simulator: 24 Träger, 0,4 s je Stellung; 1 → 21 sind 4 Stellungen rückwärts.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := dev.Handle(ctx, "fetch", json.RawMessage(`{"tray":21}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(map[string]any); r["arrived"] != true || r["tray"] != 21 {
		t.Fatalf("fetch = %v", r)
	}
	for {
		m := cl.read()
		if m.Type != "event" || m.Event != "lift.arrived" {
			continue
		}
		var d struct {
			Tray int `json:"tray"`
		}
		json.Unmarshal(m.Data, &d)
		if m.Device != "megamat" || d.Tray != 21 {
			t.Fatalf("Event = %+v", m)
		}
		break
	}
	if m := cl.call("lift.status", map[string]any{}); !m.OK {
		t.Fatalf("lift.status über WS: %+v", m.Error)
	}
}
