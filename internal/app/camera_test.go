package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/logbuf"
)

// readAny liest Text- und Binärframes; bei Binärframes kommt die Nutzlast mit.
func (cl *client) readAny() (msg, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, b, err := cl.c.Read(ctx)
	if err != nil {
		cl.t.Fatalf("lesen: %v", err)
	}
	var m msg
	if typ == websocket.MessageBinary {
		n := binary.BigEndian.Uint32(b)
		json.Unmarshal(b[4:4+n], &m)
		return m, b[4+n:]
	}
	json.Unmarshal(b, &m)
	return m, nil
}

func setupCamera(t *testing.T) (*App, int, int) {
	t.Helper()
	port, compat := freePort(t), freePort(t)
	cfg := &config.Config{
		Station:        "foto",
		ListenPort:     port,
		AllowedOrigins: []string{origin},
		Devices:        []config.DeviceConfig{{ID: "kamera", Kind: "camera", Driver: "simulator", LiveFPS: 20}},
		HTTPCompat: config.HTTPCompat{Enabled: true, Listen: fmt.Sprintf("127.0.0.1:%d", compat),
			BasicAuth: config.BasicAuth{User: "u", Password: "p"}},
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
	return a, port, compat
}

func TestWebSocketCameraCaptureAndLiveView(t *testing.T) {
	_, port, _ := setupCamera(t)
	cl, _, err := dial(t, "127.0.0.1", port, origin)
	if err != nil {
		t.Fatal(err)
	}
	cl.c.SetReadLimit(32 << 20)
	cl.hello()

	// Foto: ein Binärframe mit Antwort-Kopf und JPEG.
	id := cl.send("camera.capture", map[string]any{})
	for {
		m, payload := cl.readAny()
		if m.Type != "res" || m.ID != id {
			continue
		}
		if !m.OK || payload == nil {
			t.Fatalf("capture: %+v (%d Bytes)", m, len(payload))
		}
		var meta map[string]any
		json.Unmarshal(m.Result, &meta)
		if meta["mime"] != "image/jpeg" || meta["width"] != 2400.0 || int(meta["bytes"].(float64)) != len(payload) ||
			payload[0] != 0xFF || payload[1] != 0xD8 {
			t.Fatalf("Foto: %v, %x", meta, payload[:4])
		}
		break
	}

	// Live-Bild: nach subscribe kommen camera.frame als Binärframes.
	if m := cl.call("camera.subscribe", map[string]any{}); !m.OK {
		t.Fatalf("subscribe: %+v", m)
	}
	frames := 0
	for frames < 3 {
		m, payload := cl.readAny()
		if m.Type == "event" && m.Event == "camera.frame" {
			if m.Device != "kamera" || len(payload) < 100 || payload[0] != 0xFF {
				t.Fatalf("Frame: %+v", m)
			}
			frames++
		}
	}
}

func TestCompatShotAndHealthFromBridgeCamera(t *testing.T) {
	a, _, compat := setupCamera(t)
	deadline := time.Now().Add(3 * time.Second)
	for a.Manager().Get("kamera").Status().State != "online" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", compat)
	code, body := get(t, base+"/health", "u", "p", nil)
	if code != 200 || !strings.Contains(body, `"session_open": true`) || !strings.Contains(body, "Simulator") {
		t.Fatalf("/health: %d %s", code, body)
	}
	code, body = get(t, base+"/shot?preview=1", "u", "p", nil)
	if code != 200 || !strings.HasPrefix(body, "\xff\xd8") {
		t.Fatalf("/shot?preview=1: %d %q", code, body[:min(len(body), 40)])
	}
	if code, _ := get(t, base+"/shot", "", "", nil); code != 401 {
		t.Fatalf("/shot ohne Auth: %d", code)
	}
}
