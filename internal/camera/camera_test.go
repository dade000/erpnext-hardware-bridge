package camera

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// fake ist ein steuerbares Backend.
type fake struct {
	mu        sync.Mutex
	opens     int
	present   bool
	lostAfter int // Preview liefert ab diesem Aufruf "Kamera weg" (0 = nie)
	previews  int
	captures  int
	settings  map[string]string
}

func newFake() *fake { return &fake{present: true, settings: map[string]string{}} }

func (f *fake) opener() Opener { return func(config.DeviceConfig) Backend { return f } }

func (f *fake) Open() (Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if !f.present {
		return Info{}, Errorf(true, "camera_offline", "Keine Kamera gefunden")
	}
	return Info{Model: "Canon EOS Test", Port: "usb:001,004"}, nil
}

func (f *fake) Close() {}

func (f *fake) Capture() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures++
	return []byte("JPEG-FOTO"), nil
}

func (f *fake) Preview() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews++
	if f.lostAfter > 0 && f.previews >= f.lostAfter {
		f.present = false
		return nil, Errorf(true, "camera_offline", "I/O problem")
	}
	f.settings["viewfinder"] = "1"
	return []byte("JPEG-LIVE"), nil
}

func (f *fake) Config(name string) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "batterylevel" {
		return "75%", nil, nil
	}
	return f.settings[name], nil, nil
}

func (f *fake) SetConfig(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[name] = value
	return nil
}

func (f *fake) get(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[name]
}

func start(t *testing.T, cfg config.DeviceConfig, f *fake) (*Camera, *device.Bus) {
	t.Helper()
	bus := device.NewBus()
	cfg.ID, cfg.Kind, cfg.Driver = "kamera", Kind, "fake"
	c := NewWith(cfg, bus, slog.New(slog.NewTextHandler(io.Discard, nil)), f.opener())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitState(t, c, device.StateOnline)
	return c, bus
}

func waitState(t *testing.T, c *Camera, want device.State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.Status().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("Zustand %s statt %s (%s)", c.Status().State, want, c.Status().Message)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConnectSetsRAMTargetAndImageFormat(t *testing.T) {
	f := newFake()
	c, _ := start(t, config.DeviceConfig{ImageFormat: "Small Fine JPEG"}, f)
	if f.get("capturetarget") != "Internal RAM" || f.get("imageformat") != "Small Fine JPEG" {
		t.Fatalf("Einstellungen: %v", f.settings)
	}
	if st := c.Status(); st.Message != "Canon EOS Test · Akku 75%" || st.Port != "usb:001,004" {
		t.Fatalf("Status: %+v", st)
	}
}

func TestCaptureReturnsBinary(t *testing.T) {
	c, _ := start(t, config.DeviceConfig{}, newFake())
	res, err := c.Handle(context.Background(), "capture", nil)
	if err != nil {
		t.Fatal(err)
	}
	bin, ok := res.(*device.Binary)
	if !ok || string(bin.Data) != "JPEG-FOTO" || bin.Meta["mime"] != "image/jpeg" || bin.Meta["bytes"] != 9 {
		t.Fatalf("Ergebnis: %#v", res)
	}
}

func TestLiveFramesOnlyWhileWatchedThenViewfinderOff(t *testing.T) {
	f := newFake()
	c, bus := start(t, config.DeviceConfig{LiveFPS: 30, LiveHoldS: 1}, f)
	events, unsub := bus.Subscribe(64)
	defer unsub()

	c.AddDemand(1)
	got := 0
	timeout := time.After(3 * time.Second)
	for got < 5 {
		select {
		case ev := <-events:
			if ev.Name == "camera.frame" {
				if b := ev.Data.(*device.Binary); string(b.Data) != "JPEG-LIVE" {
					t.Fatalf("Frame: %q", b.Data)
				}
				got++
			}
		case <-timeout:
			t.Fatalf("nur %d Live-Bilder", got)
		}
	}
	if !c.Status().Last.(Last).Live {
		t.Fatal("Live nicht gemeldet")
	}

	// Foto mitten im Live-Bild.
	if _, err := c.Handle(context.Background(), "capture", nil); err != nil {
		t.Fatal(err)
	}

	c.AddDemand(-1)
	time.Sleep(200 * time.Millisecond)
	f.mu.Lock()
	n := f.previews
	f.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	if f.previews != n {
		f.mu.Unlock()
		t.Fatal("Live-Bild läuft ohne Zuschauer weiter")
	}
	f.mu.Unlock()
	if f.get("viewfinder") != "1" {
		t.Fatal("Sucher vor Ablauf der Haltezeit schon aus")
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.get("viewfinder") != "0" {
		if time.Now().After(deadline) {
			t.Fatal("Sucher nach der Haltezeit nicht aus")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLostSessionReconnects(t *testing.T) {
	f := newFake()
	f.lostAfter = 3
	c, _ := start(t, config.DeviceConfig{LiveFPS: 30}, f)
	c.AddDemand(1)
	waitState(t, c, device.StateOffline)

	// Kamera wieder da: eine Anfrage verbindet sofort neu.
	f.mu.Lock()
	f.present, f.lostAfter = true, 0
	f.mu.Unlock()
	if _, err := c.Handle(context.Background(), "capture", nil); err != nil {
		t.Fatalf("nach Wiederverbinden: %v", err)
	}
	waitState(t, c, device.StateOnline)
	if s := c.Status().Stats.(Stats); s.Reopens == 0 {
		t.Fatalf("Stats: %+v", s)
	}
}

func TestOfflineRequestFailsClearly(t *testing.T) {
	f := newFake()
	f.present = false
	bus := device.NewBus()
	c := NewWith(config.DeviceConfig{ID: "kamera", Kind: Kind, Driver: "fake"}, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)), f.opener())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitState(t, c, device.StateOffline)
	_, err := c.Handle(context.Background(), "capture", nil)
	de, ok := err.(*device.Error)
	if !ok || de.Code != "camera_offline" || de.Message != "Keine Kamera gefunden" {
		t.Fatalf("Fehler: %#v", err)
	}
}
