package metratec_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/rfid/metratec"
	"erpnext-hardware-bridge/internal/rfid/metratec/fakereader"
	"erpnext-hardware-bridge/internal/serialport"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const tidA = "E2801191A5030060A1B2C3D4"

func tagA() *fakereader.Tag {
	return &fakereader.Tag{EPC: "3034257BF468D480000003EC", TID: tidA, USR: make([]byte, 64)}
}

type harness struct {
	r    *metratec.Reader
	fake *fakereader.Reader
	bus  *device.Bus
	mu   sync.Mutex
	port *fakereader.Port
	plug bool
}

func start(t *testing.T, power int) *harness {
	t.Helper()
	h := &harness{fake: fakereader.New(), bus: device.NewBus(), plug: true}
	cfg := config.DeviceConfig{ID: "rfid", Kind: "rfid", Driver: "metratec_uhf", Port: config.PortSpec{Path: "/dev/fake"},
		Baud: 115200, PollMS: 100, PowerDBm: power}
	h.r = metratec.NewWithOpener(cfg, h.bus, quiet, func() (serialport.Port, string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.plug {
			return nil, "", serialport.ErrNotFound
		}
		h.port = h.fake.Open()
		return h.port, "/dev/fake", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go h.r.Run(ctx)
	t.Cleanup(cancel)
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
	t.Fatalf("Zustand %s erwartet, ist %s (%s)", want, d.Status().State, d.Status().Message)
}

func call(t *testing.T, d device.Device, cmd string, params any) (map[string]any, error) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	res, err := d.Handle(context.Background(), cmd, raw)
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

func TestSetup(t *testing.T) {
	h := start(t, 3)
	waitState(t, h.r, device.StateOnline)
	cmds := h.fake.Commands()
	if !slices.Contains(cmds, "AT+INVS=0,1,12,0,0,ALL,DUAL,-100") {
		t.Fatalf("Inventory-Einstellung fehlt: %v", cmds)
	}
	if h.fake.Power() != 3 {
		t.Fatalf("Sendeleistung %d, erwartet 3", h.fake.Power())
	}
	if !strings.Contains(h.r.Status().Message, "DeskID_UHF_v2") {
		t.Fatalf("Firmware nicht in der Statusmeldung: %q", h.r.Status().Message)
	}
}

func TestWrongReader(t *testing.T) {
	h := start(t, 0)
	h.fake.Hardware("DeskID_NFC")
	waitState(t, h.r, device.StateError)
}

func TestWriteURI(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	h.fake.Put(tag)

	res, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}", "offset": 4})
	if err != nil {
		t.Fatal(err)
	}
	if res["tid"] != tidA || res["uri"] != "https://holzschuhe.at/u/"+tidA || res["verified"] != true || res["read_only"] != true {
		t.Fatalf("Ergebnis %v", res)
	}
	info, err := metratec.FindType5(tag.USR)
	if err != nil || info.URI != "https://holzschuhe.at/u/"+tidA || info.Offset != 4 || !info.ReadOnly {
		t.Fatalf("Speicher: %+v %v", info, err)
	}
	// Maske wieder aus, damit das nächste Inventory alle Tags sieht.
	cmds := h.fake.Commands()
	if cmds[len(cmds)-1] != "AT+MSK=OFF" {
		t.Fatalf("letzter Befehl %q, erwartet AT+MSK=OFF", cmds[len(cmds)-1])
	}
}

func TestWriteNeedsExactlyOneTag(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	params := map[string]any{"uri": "https://holzschuhe.at/u/{tid}"}

	if _, err := call(t, h.r, "write_uri", params); code(err) != "no_tag" {
		t.Fatalf("ohne Tag: %v", err)
	}
	b := tagA()
	b.TID, b.EPC = "E2801191A5030060FFFFFFFF", "3034257BF468D480000003ED"
	h.fake.Put(tagA(), b)
	if _, err := call(t, h.r, "write_uri", params); code(err) != "multiple_tags" {
		t.Fatalf("zwei Tags: %v", err)
	}
	h.fake.Put(b)
	params["expect_tid"] = tidA
	if _, err := call(t, h.r, "write_uri", params); code(err) != "tag_changed" {
		t.Fatalf("anderer Tag: %v", err)
	}
	if h.r.Status().State != device.StateOnline {
		t.Fatalf("Bedienfehler darf den Reader nicht auf %s setzen", h.r.Status().State)
	}
}

func TestWriteFailureResetsMask(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.FailWrite = "ACCESS ERROR"
	h.fake.Put(tag)
	_, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}"})
	if code(err) != "write_failed" || !strings.Contains(err.Error(), "ACCESS ERROR") {
		t.Fatalf("Fehler %v", err)
	}
	cmds := h.fake.Commands()
	if cmds[len(cmds)-1] != "AT+MSK=OFF" {
		t.Fatalf("Maske nicht zurückgesetzt: %v", cmds[len(cmds)-3:])
	}
}

func TestWriteTooSmallMemory(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.USR = make([]byte, 16)
	h.fake.Put(tag)
	_, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}"})
	if code(err) != "write_failed" || !strings.Contains(err.Error(), "MEMORY OVERRUN") {
		t.Fatalf("Fehler %v", err)
	}
}

func TestReadDumpFindsPhoneWrittenNDEF(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	// Eine Handy-App hat den NFC-Bereich beschrieben; er liegt im
	// UHF-Nutzerspeicher ab Byte 8.
	data, _ := metratec.EncodeType5URI("https://example.com/x", false)
	copy(tag.USR[8:], data)
	h.fake.Put(tag)

	res, err := call(t, h.r, "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	ndef, _ := res["ndef"].(map[string]any)
	if ndef == nil || ndef["uri"] != "https://example.com/x" || ndef["offset"] != float64(8) {
		t.Fatalf("Auszug %v", res)
	}
	if b := region(res, 0)["bytes"]; b != float64(64) {
		t.Fatalf("64 Byte Auszug erwartet: %v", res["regions"])
	}
}

func TestReadDumpHalvesOnSmallMemory(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.USR = make([]byte, 32)
	h.fake.Put(tag)
	res, err := call(t, h.r, "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if region(res, 0)["bytes"] != float64(32) {
		t.Fatalf("32 Byte Auszug erwartet: %v", res)
	}
}

func TestSubscribePublishesChanges(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	ch, unsub := h.bus.Subscribe(16)
	defer unsub()
	h.r.AddDemand(1)
	defer h.r.AddDemand(-1)

	next := func() []any {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev := <-ch:
				if ev.Name != "rfid.tags" {
					continue
				}
				b, _ := json.Marshal(ev.Data)
				var l map[string]any
				_ = json.Unmarshal(b, &l)
				tags, _ := l["tags"].([]any)
				return tags
			case <-deadline:
				t.Fatal("kein rfid.tags-Event")
			}
		}
	}
	// Neuer Zuhörer: erst der aktuelle Stand (leer), dann Änderungen.
	if tags := next(); len(tags) != 0 {
		t.Fatalf("Anfangsstand leer erwartet: %v", tags)
	}
	h.fake.Put(tagA())
	if tags := next(); len(tags) != 1 {
		t.Fatalf("ein Tag erwartet: %v", tags)
	}
	h.fake.Put()
	if tags := next(); len(tags) != 0 {
		t.Fatalf("leer erwartet: %v", tags)
	}
}

func TestUnplugGoesOffline(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	h.mu.Lock()
	h.plug = false
	h.port.Break()
	h.mu.Unlock()
	h.r.AddDemand(1) // sofort abfragen, damit der kaputte Port auffällt
	waitState(t, h.r, device.StateOffline)
	if _, err := call(t, h.r, "inventory", nil); code(err) != "rfid_offline" {
		t.Fatalf("offline: %v", err)
	}
	h.mu.Lock()
	h.plug = true
	h.mu.Unlock()
	waitState(t, h.r, device.StateOnline)
}

func TestSilentReaderIsError(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	h.fake.Silent(true)
	if _, err := call(t, h.r, "inventory", nil); code(err) != "timeout" {
		t.Fatalf("stummer Reader: %v", err)
	}
	waitState(t, h.r, device.StateError)
}

func TestOldFirmwareReadsFullTIDAfterwards(t *testing.T) {
	h := start(t, 0)
	h.fake.OldFirmware(true)
	waitState(t, h.r, device.StateOnline)
	h.fake.Put(tagA())
	// Das Inventory liefert nur 8 Byte; geschrieben wird mit der vollen TID.
	inv, err := call(t, h.r, "inventory", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := inv["tags"].([]any)[0].(map[string]any)["tid"]; got != tidA[:16] {
		t.Fatalf("Inventory-TID %v", got)
	}
	res, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}"})
	if err != nil {
		t.Fatal(err)
	}
	if res["tid"] != tidA || res["uri"] != "https://holzschuhe.at/u/"+tidA {
		t.Fatalf("Ergebnis %v", res)
	}
	dump, err := call(t, h.r, "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump["tags"].([]any)[0].(map[string]any)["tid"]; got != tidA {
		t.Fatalf("Auszug-TID %v", got)
	}
}

func TestShortTIDIsRefused(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.TID = "E2003412" // nur Klasse/Hersteller/Modell, keine Seriennummer
	h.fake.Put(tag)
	if _, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}"}); code(err) != "no_tid" {
		t.Fatalf("kurze TID: %v", err)
	}
}

func TestReadDumpWholeMemory(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.USR = make([]byte, 136)
	data, _ := metratec.EncodeType5URI("https://example.com/weit-hinten", false)
	copy(tag.USR[72:], data)
	h.fake.Put(tag)
	res, err := call(t, h.r, "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r0 := region(res, 0); r0["bytes"] != float64(136) || r0["end"] == nil {
		t.Fatalf("Größe/Ende: %v", r0)
	}
	ndef, _ := res["ndef"].(map[string]any)
	if ndef == nil || ndef["offset"] != float64(72) {
		t.Fatalf("NDEF %v", res["ndef"])
	}
}

func region(res map[string]any, i int) map[string]any {
	list, _ := res["regions"].([]any)
	if i >= len(list) {
		return map[string]any{}
	}
	return list[i].(map[string]any)
}

// EM4425 wie geliefert: 32 Byte UHF-Nutzerspeicher, der NFC-Bereich liegt in
// derselben Bank ab Wort A0h. Ein Handy hat dort eine Adresse geschrieben.
func TestEM4425NFCAreaAtWordA0(t *testing.T) {
	for _, hfStart := range []int{320, 160} { // Byte- bzw. Wortadressierung
		t.Run(fmt.Sprint(hfStart), func(t *testing.T) {
			h := start(t, 0)
			waitState(t, h.r, device.StateOnline)
			tag := tagA()
			tag.USR = make([]byte, 32)
			tag.HF, tag.HFStart = make([]byte, 160), hfStart
			phone, _ := metratec.EncodeType5URI("https://example.com/handy", false)
			copy(tag.HF, phone)
			h.fake.Put(tag)

			res, err := call(t, h.r, "read", nil)
			if err != nil {
				t.Fatal(err)
			}
			ndef, _ := res["ndef"].(map[string]any)
			if ndef == nil || ndef["offset"] != float64(hfStart) || ndef["uri"] != "https://example.com/handy" {
				t.Fatalf("NDEF %v, Bereiche %v", res["ndef"], res["regions"])
			}
			if r1 := region(res, 1); r1["start"] != float64(hfStart) || r1["bytes"] != float64(160) {
				t.Fatalf("NFC-Bereich %v", r1)
			}

			if _, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}", "offset": hfStart}); err != nil {
				t.Fatal(err)
			}
			info, err := metratec.DecodeType5(tag.HF)
			if err != nil || info.URI != "https://holzschuhe.at/u/"+tidA {
				t.Fatalf("NFC-Bereich nach dem Schreiben: %+v %v", info, err)
			}
		})
	}
}

// Am echten Reader antwortete ein gut aufliegender Tag gelegentlich auf einen
// einzelnen Lesebefehl nicht. Das darf weder das Nachlesen der TID noch das
// Schreiben scheitern lassen.
func TestRadioDropoutsAreRetried(t *testing.T) {
	h := start(t, 0)
	h.fake.OldFirmware(true)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.USR = make([]byte, 32)
	tag.HF, tag.HFStart = make([]byte, 184), 320
	h.fake.Put(tag)

	h.fake.Miss(2) // TID-Nachlesen
	res, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}", "offset": 320})
	if err != nil {
		t.Fatal(err)
	}
	if res["tid"] != tidA {
		t.Fatalf("TID %v", res["tid"])
	}

	h.fake.Miss(3) // öfter als erlaubt
	if _, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}", "offset": 320}); code(err) != "no_tag" {
		t.Fatalf("dauerhaft kein Tag: %v", err)
	}
}

// Ein vorhandener Capability Container (wie ihn eine Handy-App anlegt) nennt
// die echte Größe des NFC-Bereichs; sie bleibt erhalten.
func TestExistingCapabilityContainerIsKept(t *testing.T) {
	h := start(t, 0)
	waitState(t, h.r, device.StateOnline)
	tag := tagA()
	tag.USR = make([]byte, 32)
	tag.HF, tag.HFStart = make([]byte, 184), 320
	copy(tag.HF, []byte{0xE1, 0x40, 0x17, 0x09, 0x03, 0x12})
	h.fake.Put(tag)
	if _, err := call(t, h.r, "write_uri", map[string]any{"uri": "https://holzschuhe.at/u/{tid}", "offset": 320}); err != nil {
		t.Fatal(err)
	}
	if tag.HF[0] != 0xE1 || tag.HF[1] != 0x43 || tag.HF[2] != 0x17 || tag.HF[3] != 0x09 {
		t.Fatalf("CC % X", tag.HF[:4])
	}
	info, err := metratec.DecodeType5(tag.HF)
	if err != nil || info.URI != "https://holzschuhe.at/u/"+tidA || !info.ReadOnly {
		t.Fatalf("%+v %v", info, err)
	}
}
