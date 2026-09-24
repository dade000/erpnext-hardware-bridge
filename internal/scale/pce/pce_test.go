package pce_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/scale/pce"
	"erpnext-hardware-bridge/internal/scale/pce/fakescale"
	"erpnext-hardware-bridge/internal/serialport"
)

func testCfg() config.DeviceConfig {
	return config.DeviceConfig{ID: "waage", Kind: "scale", Driver: "pce_pb", Port: config.PortSpec{Path: "/dev/fake"},
		Baud: 9600, PollMS: 50, StableSamples: 3, StableToleranceKG: 0.005}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type harness struct {
	scale  *pce.Scale
	fake   *fakescale.Scale
	bus    *device.Bus
	cancel context.CancelFunc
	mu     sync.Mutex
	port   *fakescale.Port
	plug   bool
}

func start(t *testing.T, kg float64) *harness {
	t.Helper()
	h := &harness{fake: fakescale.New(kg), bus: device.NewBus(), plug: true}
	h.scale = pce.NewWithOpener(testCfg(), h.bus, quiet, func() (serialport.Port, string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.plug {
			return nil, "", serialport.ErrNotFound
		}
		h.port = h.fake.Open()
		return h.port, "/dev/fake", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.scale.Run(ctx)
	t.Cleanup(cancel)
	waitState(t, h.scale, device.StateOnline)
	return h
}

func waitState(t *testing.T, d device.Device, want device.State) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if d.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Zustand %s nicht erreicht, ist %s (%s)", want, d.Status().State, d.Status().Message)
}

func TestReadAndTare(t *testing.T) {
	h := start(t, 1.56)
	res, err := h.scale.Handle(context.Background(), "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rd := res.(pce.Reading); rd.KG != 1.56 || rd.Unit != "kg" {
		t.Fatalf("Messung %+v", rd)
	}
	if _, err := h.scale.Handle(context.Background(), "tare", nil); err != nil {
		t.Fatal(err)
	}
	res, _ = h.scale.Handle(context.Background(), "read", nil)
	if rd := res.(pce.Reading); rd.KG != 0 {
		t.Fatalf("nach Tara %v kg", rd.KG)
	}
	found := false
	for _, c := range h.fake.Commands() {
		found = found || c == "ST"
	}
	if !found {
		t.Fatal("ST wurde nicht gesendet")
	}
}

func TestLiveEventsAndStable(t *testing.T) {
	h := start(t, 2.0)
	events, unsub := h.bus.Subscribe(64)
	defer unsub()
	h.scale.AddDemand(1)
	defer h.scale.AddDemand(-1)

	var got []pce.Reading
	timeout := time.After(3 * time.Second)
	for len(got) < 4 {
		select {
		case ev := <-events:
			if ev.Name == "scale.weight" {
				got = append(got, ev.Data.(pce.Reading))
			}
		case <-timeout:
			t.Fatalf("nur %d Messungen erhalten", len(got))
		}
	}
	if !got[len(got)-1].Stable {
		t.Fatalf("ruhendes Gewicht nicht als stabil erkannt: %+v", got)
	}
}

func TestSilentScaleGoesErrorThenRecovers(t *testing.T) {
	h := start(t, 1)
	h.fake.Silent(true)
	_, err := h.scale.Handle(context.Background(), "read", nil)
	var de *device.Error
	if !errors.As(err, &de) || de.Code != "timeout" {
		t.Fatalf("erwartet timeout, bekam %v", err)
	}
	if h.scale.Status().State != device.StateError {
		t.Fatalf("Zustand %s", h.scale.Status().State)
	}
	h.fake.Silent(false)
	if _, err := h.scale.Handle(context.Background(), "read", nil); err != nil {
		t.Fatal(err)
	}
	waitState(t, h.scale, device.StateOnline)
}

func TestUnplugReportsOfflineAndReconnects(t *testing.T) {
	h := start(t, 1)
	h.mu.Lock()
	h.plug = false
	h.port.Break()
	h.mu.Unlock()
	h.scale.AddDemand(1) // sofort messen statt 5 s Leerlauf
	defer h.scale.AddDemand(-1)
	waitState(t, h.scale, device.StateOffline)

	_, err := h.scale.Handle(context.Background(), "read", nil)
	var de *device.Error
	if !errors.As(err, &de) || de.Code != "scale_offline" {
		t.Fatalf("erwartet scale_offline, bekam %v", err)
	}

	h.mu.Lock()
	h.plug = true
	h.mu.Unlock()
	waitState(t, h.scale, device.StateOnline)
}

func TestGarbageIsBadResponse(t *testing.T) {
	h := start(t, 1)
	h.fake.Garbage("E-01")
	_, err := h.scale.Handle(context.Background(), "read", nil)
	var de *device.Error
	if !errors.As(err, &de) || de.Code != "bad_response" {
		t.Fatalf("erwartet bad_response, bekam %v", err)
	}
}
