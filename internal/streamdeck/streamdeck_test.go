package streamdeck

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// fakeConn ist ein nachgebautes MK.2: Input-Reports kommen aus in, jedes
// fertig übertragene Tastenbild landet in images.
type fakeConn struct {
	in     chan []byte
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	features [][]byte
	pending  map[byte][]byte
	images   map[byte]int // Taste → Zahl der gezeichneten Bilder
	writeErr error
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan []byte, 8), closed: make(chan struct{}), pending: map[byte][]byte{}, images: map[byte]int{}}
}

func (f *fakeConn) Read(p []byte) (int, error) {
	select {
	case r := <-f.in:
		return copy(p, r), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeConn) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	if len(p) != imageReportLen || p[0] != 0x02 || p[1] != 0x07 {
		return errors.New("falscher Report")
	}
	key, last := p[2], p[3] == 1
	n := binary.LittleEndian.Uint16(p[4:])
	f.pending[key] = append(f.pending[key], p[imageHeaderLen:imageHeaderLen+int(n)]...)
	if last {
		if !bytes.HasPrefix(f.pending[key], []byte{0xff, 0xd8}) {
			return errors.New("kein JPEG")
		}
		f.images[key]++
		delete(f.pending, key)
	}
	return nil
}

func (f *fakeConn) SetFeature(p []byte) error {
	f.mu.Lock()
	f.features = append(f.features, append([]byte(nil), p...))
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) GetFeature(p []byte) (int, error) {
	switch p[0] {
	case 0x05:
		copy(p[6:], "1.00.012")
	case 0x06:
		copy(p[2:], "AL12K2C01234")
	}
	return featureLen, nil
}

func (f *fakeConn) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeConn) drawn(key byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[key]
}

func keyReport(down ...int) []byte {
	r := make([]byte, inputReportLen)
	r[0], r[2] = 0x01, 15
	for _, k := range down {
		r[keyOffset+k] = 1
	}
	return r
}

func testDeck(t *testing.T, conn *fakeConn) (*Deck, *device.Bus, context.CancelFunc) {
	t.Helper()
	bus := device.NewBus()
	cfg := config.DeviceConfig{ID: "deck", Kind: Kind, Driver: "elgato", Keys: []config.DeckKey{
		{Key: 0, Label: "Foto", Action: "photo.shoot", Color: "#2e7d32"},
		{Key: 4, Label: "Nur ein Schild"},
	}}
	opened := false
	d := NewWithOpener(cfg, bus, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string) (Conn, Info, error) {
		if opened {
			return nil, Info{}, ErrNotFound
		}
		opened = true
		return conn, Info{Path: "/dev/hidraw9", PID: 0x0080}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)
	waitFor(t, func() bool { return d.Status().State == device.StateOnline })
	return d, bus, cancel
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("Zeitüberschreitung")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPressPublishesAction(t *testing.T) {
	conn := newFakeConn()
	d, bus, cancel := testDeck(t, conn)
	defer cancel()
	events, unsub := bus.Subscribe(16)
	defer unsub()

	st := d.Status()
	if last := st.Last.(Last); last.Serial != "AL12K2C01234" || last.Firmware != "1.00.012" || last.Model != "Stream Deck MK.2" {
		t.Fatalf("Last = %+v", last)
	}
	for k := byte(0); k < 15; k++ {
		if conn.drawn(k) != 1 {
			t.Fatalf("Taste %d: %d Bilder nach dem Verbinden", k, conn.drawn(k))
		}
	}

	conn.in <- keyReport(0)
	conn.in <- keyReport()
	conn.in <- keyReport(4) // Taste ohne Aktion: kein Event
	conn.in <- keyReport()
	var got []device.Event
	timeout := time.After(time.Second)
	for len(got) < 1 {
		select {
		case ev := <-events:
			if ev.Name == "streamdeck.key" {
				got = append(got, ev)
			}
		case <-timeout:
			t.Fatal("kein streamdeck.key")
		}
	}
	data := got[0].Data.(map[string]any)
	if got[0].Device != "deck" || data["action"] != "photo.shoot" || data["key"] != 0 {
		t.Fatalf("Event = %+v", got[0])
	}
	// Gedrückt und losgelassen: zwei neue Bilder für Taste 0.
	waitFor(t, func() bool { return conn.drawn(0) == 3 && conn.drawn(4) == 3 })
	select {
	case ev := <-events:
		if ev.Name == "streamdeck.key" {
			t.Fatalf("Taste ohne Aktion meldet %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
	}
	waitFor(t, func() bool { return d.Status().Stats.(Stats).Presses == 2 })
}

func TestDemandRedraws(t *testing.T) {
	conn := newFakeConn()
	d, _, cancel := testDeck(t, conn)
	defer cancel()
	d.AddDemand(1) // niemand → jemand: alles neu
	waitFor(t, func() bool { return conn.drawn(14) == 2 })
	d.AddDemand(1) // jemand → jemand: nichts
	d.AddDemand(-1)
	d.AddDemand(-1) // jemand → niemand: alles neu (grau)
	waitFor(t, func() bool { return conn.drawn(14) == 3 })
	time.Sleep(30 * time.Millisecond)
	if n := conn.drawn(14); n != 3 {
		t.Fatalf("Taste 15: %d Bilder, erwartet 3", n)
	}
}

func TestSimulatedPressAndTest(t *testing.T) {
	conn := newFakeConn()
	d, bus, cancel := testDeck(t, conn)
	defer cancel()
	events, unsub := bus.Subscribe(16)
	defer unsub()
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()

	res, err := d.Handle(ctx, "press", json.RawMessage(`{"key":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["action"] != "photo.shoot" {
		t.Fatalf("press = %+v", res)
	}
	select {
	case ev := <-events:
		if ev.Name != "streamdeck.key" {
			t.Fatalf("Event %s", ev.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("kein Event")
	}
	waitFor(t, func() bool { return conn.drawn(0) == 3 }) // gedrückt, dann wieder normal

	if _, err := d.Handle(ctx, "press", json.RawMessage(`{"key":15}`)); err == nil {
		t.Fatal("Taste 16 angenommen")
	}

	res, err = d.Handle(ctx, "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := res.(map[string]any)
	keys := r["keys"].([]map[string]any)
	if len(keys) != 15 || !strings.HasPrefix(keys[0]["image"].(string), "data:image/png;base64,") || r["serial"] != "AL12K2C01234" {
		t.Fatalf("test = %+v", r)
	}
	if conn.drawn(14) != 2 {
		t.Fatalf("Tastennummern nicht gezeichnet: %d", conn.drawn(14))
	}
}

func TestUnplugGoesOffline(t *testing.T) {
	conn := newFakeConn()
	d, _, cancel := testDeck(t, conn)
	defer cancel()
	conn.Close() // Read liefert EOF wie beim Abziehen
	waitFor(t, func() bool { return d.Status().State == device.StateOffline })
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	_, err := d.Handle(ctx, "press", json.RawMessage(`{"key":0}`))
	var de *device.Error
	if !errors.As(err, &de) || de.Code != "streamdeck_offline" {
		t.Fatalf("err = %v", err)
	}
}

func TestStopResetsDeck(t *testing.T) {
	conn := newFakeConn()
	_, _, cancel := testDeck(t, conn)
	cancel()
	waitFor(t, func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		l := conn.features[len(conn.features)-1]
		return l[0] == 0x03 && l[1] == 0x02
	})
	if conn.features[0][1] != 0x08 || conn.features[0][2] != defaultBrightness {
		t.Fatalf("Helligkeit = % x", conn.features[0][:3])
	}
}

func TestImageReports(t *testing.T) {
	img := bytes.Repeat([]byte{0xab}, 2500)
	rs := imageReports(7, img)
	if len(rs) != 3 {
		t.Fatalf("%d Reports", len(rs))
	}
	var back []byte
	for i, r := range rs {
		if len(r) != 1024 || r[2] != 7 || int(binary.LittleEndian.Uint16(r[6:])) != i || (r[3] == 1) != (i == 2) {
			t.Fatalf("Report %d: % x", i, r[:8])
		}
		back = append(back, r[8:8+binary.LittleEndian.Uint16(r[4:])]...)
	}
	if !bytes.Equal(back, img) {
		t.Fatal("Nutzlast verändert")
	}
}

func TestRenderKey(t *testing.T) {
	k := &config.DeckKey{Label: "Eingetroffen + Etikett drucken", Color: "#ffffff"}
	img := renderKey(k, 0, 72, lookNormal)
	dark := 0
	for y := 0; y < 72; y++ {
		for x := 0; x < 72; x++ {
			if img.RGBAAt(x, y).R < 100 {
				dark++
			}
		}
	}
	if dark < 50 {
		t.Fatalf("kaum Schrift auf weißem Grund (%d dunkle Pixel)", dark)
	}
	f, lines := layout("Eingetroffen + Etikett drucken", 60, 60)
	if f == nil || len(lines) < 2 {
		t.Fatalf("Zeilen = %q", lines)
	}
	if got := flip(img).RGBAAt(71, 71); got != img.RGBAAt(0, 0) {
		t.Fatal("flip spiegelt nicht beide Achsen")
	}
	if got := printable("Weiter ✔ Größe"); got != "Weiter  Größe" {
		t.Fatalf("printable = %q", got)
	}
	if idle := renderKey(&config.DeckKey{Color: "#ff0000"}, 0, 72, lookIdle).RGBAAt(36, 36); idle.R != idle.G {
		t.Fatalf("grau erwartet, %v", idle)
	}
}
