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

// Seite abonniert das Stream Deck, ein Druck kommt als streamdeck.key mit
// der Aktion an; ohne Abo kommt nichts.
func TestWebSocketStreamDeckKey(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Station: "foto", ListenPort: port, AllowedOrigins: []string{origin},
		Devices: []config.DeviceConfig{{ID: "deck", Kind: "streamdeck", Driver: "simulator",
			Keys: []config.DeckKey{{Key: 2, Label: "Auslösen", Action: "photo.shoot"}}}},
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

	cl, _, err := dial(t, "127.0.0.1", port, origin)
	if err != nil {
		t.Fatal(err)
	}
	cl.hello()
	if m := cl.call("streamdeck.subscribe", map[string]any{}); !m.OK {
		t.Fatalf("subscribe: %+v", m.Error)
	}
	dev := a.Manager().Get("deck")
	deadline := time.Now().Add(2 * time.Second)
	for dev.Status().State != "online" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := dev.Handle(ctx, "press", json.RawMessage(`{"key":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := res.(map[string]any)["listeners"]; n != int32(1) {
		t.Fatalf("listeners = %v", n)
	}
	for {
		m := cl.read()
		if m.Type != "event" || m.Event != "streamdeck.key" {
			continue
		}
		var d struct {
			Key    int    `json:"key"`
			Action string `json:"action"`
		}
		json.Unmarshal(m.Data, &d)
		if m.Device != "deck" || d.Key != 2 || d.Action != "photo.shoot" {
			t.Fatalf("Event = %+v", m)
		}
		break
	}
	if m := cl.call("streamdeck.press", map[string]any{"key": 2}); !m.OK {
		t.Fatalf("streamdeck.press über WS: %+v", m.Error)
	}
}
