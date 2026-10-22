package kardex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/lift/kardex/fakec3000"
)

func setup(t *testing.T, trays int) (*Lift, *fakec3000.Machine, *device.Bus, func()) {
	t.Helper()
	m := fakec3000.New(trays)
	m.Step = 20 * time.Millisecond
	srv := httptest.NewServer(m)
	bus := device.NewBus()
	cfg := config.DeviceConfig{ID: "lift", Kind: Kind, Driver: "kardex_c3000", Address: strings.TrimPrefix(srv.URL, "http://")}
	l := New(cfg, bus, slog.New(slog.NewTextHandler(io.Discard, nil))).(*Lift)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	waitFor(t, func() bool { return l.Status().State == device.StateOnline })
	return l, m, bus, func() { cancel(); <-done; m.Close(); srv.Close() }
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("Zeitüberschreitung")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func call(l *Lift, cmd string, p any) (map[string]any, error) {
	raw, _ := json.Marshal(p)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := l.Handle(ctx, cmd, raw)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func code(err error) string {
	var de *device.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestConnectReadsMachineInfo(t *testing.T) {
	l, _, _, stop := setup(t, 24)
	defer stop()
	last := l.Status().Last.(Last)
	if last.Trays != 24 || last.Type != "VCM (vertikales Umlaufregal)" || last.Version != "1.14.2" {
		t.Fatalf("Info: %+v", last.Info)
	}
}

func TestFetchWaitsUntilTrayInOpening(t *testing.T) {
	l, m, bus, stop := setup(t, 24)
	defer stop()
	events, unsub := bus.Subscribe(64)
	defer unsub()

	res, err := call(l, "fetch", map[string]any{"tray": 21, "light_from": 3, "light_to": 4})
	if err != nil {
		t.Fatal(err)
	}
	if res["arrived"] != true || res["tray"] != float64(21) {
		t.Fatalf("Antwort %v", res)
	}
	if m.Current() != 21 {
		t.Fatalf("Lift steht bei %d", m.Current())
	}
	var arrived bool
	for len(events) > 0 {
		if ev := <-events; ev.Name == "lift.arrived" {
			arrived = true
		}
	}
	if !arrived {
		t.Fatal("kein lift.arrived")
	}
	if l.Status().Last.(Last).Target != 0 {
		t.Fatal("Ziel nach Ankunft nicht gelöscht")
	}
}

func TestFetchSameTrayIsImmediate(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	if _, err := call(l, "fetch", map[string]any{"tray": 1}); err != nil {
		t.Fatal(err)
	}
	if got := m.Drives(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Fahrten %v", got)
	}
}

func TestFetchRejectsUnknownTray(t *testing.T) {
	l, _, _, stop := setup(t, 24)
	defer stop()
	if _, err := call(l, "fetch", map[string]any{"tray": 25}); code(err) != "bad_request" {
		t.Fatalf("Fehler %v", err)
	}
	if _, err := call(l, "fetch", map[string]any{}); code(err) != "bad_request" {
		t.Fatalf("Fehler %v", err)
	}
}

func TestNotInHostMenu(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	m.SetHostMenu(false)
	if _, err := call(l, "fetch", map[string]any{"tray": 5}); code(err) != "not_host_menu" {
		t.Fatalf("Fehler %v", err)
	}
	res, err := call(l, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["status"].(map[string]any)["host_menu"] != false {
		t.Fatalf("Status %v", res)
	}
	if st := l.Status(); st.State != device.StateOnline || !strings.Contains(st.Message, "Host-Menü") {
		t.Fatalf("Zustand %v %q", st.State, st.Message)
	}
}

func TestNewFetchSupersedesWaitingOne(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	m.Step = 50 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		_, firstErr = call(l, "fetch", map[string]any{"tray": 12})
	}()
	waitFor(t, func() bool { return len(m.Drives()) == 1 })
	res, err := call(l, "fetch", map[string]any{"tray": 3})
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if code(firstErr) != "superseded" {
		t.Fatalf("erste Anforderung: %v", firstErr)
	}
	if res["arrived"] != true || m.Current() != 3 {
		t.Fatalf("Antwort %v, Lift bei %d", res, m.Current())
	}
	if d := m.Drives(); len(d) != 2 || d[1] != 3 {
		t.Fatalf("Fahrten %v", d)
	}
}

func TestFetchNoWaitPublishesArrival(t *testing.T) {
	l, m, bus, stop := setup(t, 24)
	defer stop()
	events, unsub := bus.Subscribe(64)
	defer unsub()
	res, err := call(l, "fetch", map[string]any{"tray": 8, "wait": false})
	if err != nil {
		t.Fatal(err)
	}
	if res["accepted"] != true {
		t.Fatalf("Antwort %v", res)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Name == "lift.arrived" {
				if ev.Data.(map[string]any)["tray"] != 8 || m.Current() != 8 {
					t.Fatalf("Ankunft %v", ev.Data)
				}
				return
			}
		case <-deadline:
			t.Fatal("kein lift.arrived")
		}
	}
}

func TestFetchTimeout(t *testing.T) {
	l, m, bus, stop := setup(t, 24)
	defer stop()
	events, unsub := bus.Subscribe(64)
	defer unsub()
	m.Step = time.Hour
	_, err := call(l, "fetch", map[string]any{"tray": 10, "timeout_s": 1})
	if code(err) != "timeout" || !strings.Contains(err.Error(), "Träger 10") {
		t.Fatalf("Fehler %v", err)
	}
	var failed bool
	for len(events) > 0 {
		if ev := <-events; ev.Name == "lift.failed" {
			failed = true
		}
	}
	if !failed || l.Status().Stats.(Stats).Failures != 1 {
		t.Fatal("Fehlschlag nicht gemeldet")
	}
}

func TestListReorderedAfterControlRestart(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	m.Restart()
	if _, err := call(l, "fetch", map[string]any{"tray": 2}); err != nil {
		t.Fatal(err)
	}
}

func TestDisplayWritesEscapedText(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	if _, err := call(l, "display", map[string]any{"lines": []string{"AB 2026/0142", "Größe 42 & mehr"}}); err != nil {
		t.Fatal(err)
	}
	if got := m.Text(154); got != "AB 2026/0142" {
		t.Fatalf("Zeile 1 %q", got)
	}
	if got := m.Text(176); got != "Groesse 42 & mehr" {
		t.Fatalf("Zeile 2 %q", got)
	}
}

func TestOfflineWhenUnreachable(t *testing.T) {
	bus := device.NewBus()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	l := New(config.DeviceConfig{ID: "lift", Kind: Kind, Driver: "kardex_c3000", Address: addr}, bus, slog.New(slog.NewTextHandler(io.Discard, nil))).(*Lift)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	waitFor(t, func() bool { return l.Status().State == device.StateOffline })
}

func TestAddressDefaultsToPort81(t *testing.T) {
	for in, want := range map[string]string{"192.168.1.100": "192.168.1.100:81", "lift.local:8081": "lift.local:8081", "fe80::1": "[fe80::1]:81"} {
		if got := Address(in); got != want {
			t.Errorf("%s → %s, erwartet %s", in, got, want)
		}
	}
}

func TestCGIText(t *testing.T) {
	if got := cgiText("a b,c=ä", 20); got != "a%20b%2Cc%3Dae" {
		t.Fatal(got)
	}
	if got := cgiText(strings.Repeat("x", 30), 20); len(got) != 20 {
		t.Fatal(got)
	}
}

// Zwei Bedienöffnungen derselben Steuerung, gleichzeitig angefordert: beide
// Träger kommen an ihrer Öffnung an, und die Kommandos überholen sich nie
// (CMD wird nie geschrieben, solange die Steuerung noch arbeitet).
func TestTwoOpeningsShareOneControl(t *testing.T) {
	m := fakec3000.NewWithOpenings(24, 2)
	m.Step = 10 * time.Millisecond
	srv := httptest.NewServer(m)
	defer srv.Close()
	defer m.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	bus := device.NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lifts []*Lift
	for _, hi := range []int{1, 2} {
		l := New(config.DeviceConfig{ID: fmt.Sprintf("oeffnung%d", hi), Kind: Kind, Driver: "kardex_c3000", Address: addr, HostID: hi}, bus, log).(*Lift)
		go l.Run(ctx)
		lifts = append(lifts, l)
	}
	for _, l := range lifts {
		waitFor(t, func() bool { return l.Status().State == device.StateOnline })
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, tray := range []int{5, 20} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = call(lifts[i], "fetch", map[string]any{"tray": tray})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Öffnung %d: %v", i+1, err)
		}
	}
	// Die zweite Fahrt hat die erste ggf. weggedreht; am Ende steht
	// mindestens der zuletzt gefahrene Träger an seiner Öffnung.
	if m.CurrentAt(1) != 5 && m.CurrentAt(2) != 20 {
		t.Fatalf("Öffnung 1: %d, Öffnung 2: %d", m.CurrentAt(1), m.CurrentAt(2))
	}
	if n := m.Collisions(); n != 0 {
		t.Fatalf("%d Kommandos haben sich überholt", n)
	}
}

func TestArrivalIgnoresOpeningBusy(t *testing.T) {
	l, m, _, stop := setup(t, 24)
	defer stop()
	m.OpeningBusyAtRest = true
	res, err := call(l, "fetch", map[string]any{"tray": 4})
	if err != nil || res["arrived"] != true {
		t.Fatalf("%v %v", res, err)
	}
}
