package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
station: parcel-1
allowed_origins:
  - https://ERP.holzschuhe.at/
devices:
  - id: waage
    kind: scale
    driver: pce_pb
    port: /dev/waage
  - id: waage2
    kind: scale
    driver: pce_pb
    port:
      match: {vid: "067b", pid: "2303"}
http_compat:
  enabled: true
  listen: ":5000"
`

func TestLoadDefaultsAndShortPort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bridge.yaml")
	os.WriteFile(p, []byte(sample), 0o600)
	c, exists, err := Load(p)
	if err != nil || !exists {
		t.Fatal(err)
	}
	if c.ListenPort != DefaultListenPort || c.Devices[0].Baud != 9600 || c.Devices[0].PollMS != 250 {
		t.Fatalf("Defaults fehlen: %+v", c)
	}
	if c.Devices[0].Port.Path != "/dev/waage" || c.Devices[1].Port.Match.VID != "067b" {
		t.Fatalf("Port falsch: %+v", c.Devices)
	}
	if c.AllowedOrigins[0] != "https://erp.holzschuhe.at" {
		t.Fatalf("Origin nicht normalisiert: %q", c.AllowedOrigins[0])
	}
}

func TestMissingFileGivesDefault(t *testing.T) {
	c, exists, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || exists || c.ListenPort != DefaultListenPort {
		t.Fatal(exists, err)
	}
}

func TestValidateCollectsErrors(t *testing.T) {
	c := &Config{ListenPort: 5000, LogLevel: "info",
		AllowedOrigins: []string{"erp.holzschuhe.at", "https://x.at/path"},
		Devices: []DeviceConfig{
			{ID: "Waage!", Kind: "scale", Driver: "pce_pb"},
			{ID: "a", Kind: "toaster", Driver: "x"},
			{ID: "a", Kind: "scale", Driver: "pce_pb", Port: PortSpec{Path: "/dev/x"}, Baud: 9600, PollMS: 10, StableSamples: 1},
		},
		HTTPCompat: HTTPCompat{Enabled: true, Listen: ":5000", BasicAuth: BasicAuth{User: "u"}},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("erwartet Fehler")
	}
	for _, want := range []string{"kein Origin", "keinen Pfad", "id darf nur", "toaster", "doppelt", "poll_ms", "port fehlt", "Port der WS-API", "nur gemeinsam"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fehler %q fehlt in:\n%v", want, err)
		}
	}
}

func TestValidatePrinters(t *testing.T) {
	good := &Config{ListenPort: 8735, LogLevel: "info", Devices: []DeviceConfig{
		{ID: "netz", Kind: "printer", Driver: "raw_tcp", Address: "192.168.1.60:9100"},
		{ID: "usb", Kind: "printer", Driver: "raw_file", Port: PortSpec{Path: "/dev/usb/lp0"}},
		{ID: "os", Kind: "printer", Driver: "system", Queue: "Zebra_ZD421"},
		{ID: "buero", Kind: "printer", Driver: "ipp", URI: "ipps://drucker.lan/ipp/print", PrintScaling: "fit", Default: true},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("gültige Drucker abgelehnt: %v", err)
	}
	bad := &Config{ListenPort: 8735, LogLevel: "info", Devices: []DeviceConfig{
		{ID: "a", Kind: "printer", Driver: "raw_tcp", Address: "192.168.1.60"},
		{ID: "b", Kind: "printer", Driver: "raw_tcp", Address: "host:99999"},
		{ID: "c", Kind: "printer", Driver: "raw_file"},
		{ID: "d", Kind: "printer", Driver: "system", Queue: "  "},
		{ID: "e", Kind: "printer", Driver: "ipp", URI: "https://drucker.lan/ipp/print"},
		{ID: "f", Kind: "printer", Driver: "ipp", URI: "ipp://drucker.lan/ipp/print", PrintScaling: "klein"},
	}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("erwartet Fehler")
	}
	for _, want := range []string{"nicht host:port", "keinen gültigen Port", "port.path fehlt", "queue fehlt", "keine ipp://", "print_scaling"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fehler %q fehlt in:\n%v", want, err)
		}
	}
}

func TestValidateTerminalForwards(t *testing.T) {
	good := &Config{ListenPort: 8735, LogLevel: "info", Devices: []DeviceConfig{
		{ID: "terminal", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.50:80", LocalPort: 8736},
		{ID: "terminal2", Kind: "terminal", Driver: "ws_forward", Address: "terminal2.lan:80", LocalPort: 8737},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("gültige Weiterleitungen abgelehnt: %v", err)
	}
	bad := &Config{ListenPort: 8735, LogLevel: "info", Devices: []DeviceConfig{
		{ID: "a", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.50", LocalPort: 8736},
		{ID: "b", Kind: "terminal", Driver: "ws_forward", Address: "127.0.0.1:80", LocalPort: 8737},
		{ID: "c", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.51:80", LocalPort: 8735},
		{ID: "d", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.52:80", LocalPort: 8736},
		{ID: "e", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.53:80"},
		{ID: "f", Kind: "terminal", Driver: "ws_forward", Address: "192.168.1.54:80", LocalPort: 5000},
	}, HTTPCompat: HTTPCompat{Enabled: true, Listen: ":5000"}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("erwartet Fehler")
	}
	for _, want := range []string{"nicht host:port", "zeigt auf diesen Rechner", "Port der WS-API", `schon "a" zugeordnet`, "local_port 0", "Terminal-Weiterleitung \"f\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fehler %q fehlt in:\n%v", want, err)
		}
	}
}

func TestSaveAtomicWithBackup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "bridge.yaml")
	c := Default()
	c.AllowedOrigins = []string{"https://erp.holzschuhe.at"}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	c.Station = "zwei"
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	bak, _ := os.ReadFile(p + ".bak")
	cur, _ := os.ReadFile(p)
	if !strings.Contains(string(cur), "station: zwei") || strings.Contains(string(bak), "station: zwei") {
		t.Fatalf("Sicherung falsch:\n%s\n---\n%s", bak, cur)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("Rechte %v", st.Mode().Perm())
	}
	got, _, err := Load(p)
	if err != nil || got.Station != "zwei" {
		t.Fatal(err)
	}
}
