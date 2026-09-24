package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/logbuf"
	"erpnext-hardware-bridge/internal/scale/pce"
	"erpnext-hardware-bridge/internal/scale/pce/fakescale"
	"erpnext-hardware-bridge/internal/serialport"
)

const origin = "https://erp.test"

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type env struct {
	app    *App
	port   int
	compat int
	fake   *fakescale.Scale
	legacy *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{port: freePort(t), compat: freePort(t), fake: fakescale.New(1.56)}
	e.legacy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "legacy %s", r.URL.Path)
	}))
	t.Cleanup(e.legacy.Close)

	cfg := &config.Config{
		Station:        "test",
		ListenPort:     e.port,
		AllowedOrigins: []string{origin},
		Devices: []config.DeviceConfig{{ID: "waage", Kind: "scale", Driver: "fake", Port: config.PortSpec{Path: "/dev/fake"},
			Baud: 9600, PollMS: 50, StableSamples: 2, StableToleranceKG: 0.005}},
		HTTPCompat: config.HTTPCompat{Enabled: true, Listen: fmt.Sprintf("127.0.0.1:%d", e.compat),
			BasicAuth: config.BasicAuth{User: "u", Password: "p"}, LegacyUpstream: e.legacy.URL},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ring := logbuf.NewRing(100)
	log, lv := logbuf.Setup(ring)
	log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.app = New(filepath.Join(t.TempDir(), "bridge.yaml"), cfg, false, log, lv, ring)
	e.app.Manager().Register("scale", "fake", func(c config.DeviceConfig, bus *device.Bus, l *slog.Logger) device.Device {
		return pce.NewWithOpener(c, bus, l, func() (serialport.Port, string, error) { return e.fake.Open(), "/dev/fake", nil })
	})
	if err := e.app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		e.app.Shutdown(ctx)
	})
	return e
}

type client struct {
	t    *testing.T
	c    *websocket.Conn
	next int
}

func dial(t *testing.T, host string, port int, org string) (*client, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws", net.JoinHostPort(host, fmt.Sprint(port))),
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {org}}})
	if err != nil {
		return nil, resp, err
	}
	t.Cleanup(func() { c.CloseNow() })
	return &client{t: t, c: c}, resp, nil
}

func (cl *client) send(method string, params any) int {
	cl.next++
	b, _ := json.Marshal(map[string]any{"id": cl.next, "type": "req", "method": method, "params": params})
	if err := cl.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		cl.t.Fatal(err)
	}
	return cl.next
}

type msg struct {
	ID     int             `json:"id"`
	Type   string          `json:"type"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *device.Error   `json:"error"`
	Event  string          `json:"event"`
	Device string          `json:"device"`
	Data   json.RawMessage `json:"data"`
}

func (cl *client) read() msg {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := cl.c.Read(ctx)
	if err != nil {
		cl.t.Fatalf("lesen: %v", err)
	}
	var m msg
	json.Unmarshal(b, &m)
	return m
}

// call wartet auf die Antwort und überspringt Events.
func (cl *client) call(method string, params any) msg {
	id := cl.send(method, params)
	for {
		if m := cl.read(); m.Type == "res" && m.ID == id {
			return m
		}
	}
}

func (cl *client) hello() msg {
	return cl.call("hello", map[string]any{"client": "test", "protocol": 1})
}

func TestWebSocketLiveWeight(t *testing.T) {
	e := setup(t)
	cl, _, err := dial(t, "localhost", e.port, origin)
	if err != nil {
		t.Fatal(err)
	}
	h := cl.hello()
	if !h.OK || !strings.Contains(string(h.Result), `"waage"`) {
		t.Fatalf("hello: %+v %s", h, h.Result)
	}
	// Ohne Geräte-ID: erste Waage. Das Desk muss die ID nicht kennen.
	if r := cl.call("scale.subscribe", nil); !r.OK {
		t.Fatalf("subscribe: %+v", r.Error)
	}
	var last pce.Reading
	deadline := time.Now().Add(4 * time.Second)
	for !last.Stable && time.Now().Before(deadline) {
		if m := cl.read(); m.Event == "scale.weight" {
			json.Unmarshal(m.Data, &last)
		}
	}
	if last.KG != 1.56 || !last.Stable {
		t.Fatalf("Live-Gewicht %+v", last)
	}
	e.fake.Set(3.2)
	for last.KG != 3.2 {
		if m := cl.read(); m.Event == "scale.weight" {
			json.Unmarshal(m.Data, &last)
		}
	}
	r := cl.call("scale.read", map[string]string{"device": "waage"})
	if !r.OK || !strings.Contains(string(r.Result), `"kg":3.2`) {
		t.Fatalf("read: %+v %s", r.Error, r.Result)
	}
	if r := cl.call("scale.read", map[string]string{"device": "gibtsnicht"}); r.OK || r.Error.Code != "device_missing" {
		t.Fatalf("unbekanntes Gerät: %+v", r)
	}
}

func TestWebSocketIPv6Loopback(t *testing.T) {
	e := setup(t)
	if l, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("kein IPv6-Loopback in dieser Umgebung")
	} else {
		l.Close()
	}
	cl, _, err := dial(t, "::1", e.port, origin)
	if err != nil {
		t.Fatal(err)
	}
	if h := cl.hello(); !h.OK {
		t.Fatalf("hello über ::1: %+v", h.Error)
	}
}

func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	e := setup(t)
	_, resp, err := dial(t, "localhost", e.port, "https://evil.test")
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("fremder Origin nicht abgelehnt: %v", err)
	}
	_, resp, err = dial(t, "localhost", e.port, "")
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("fehlender Origin nicht abgelehnt: %v", err)
	}
}

func TestWebSocketRequiresHello(t *testing.T) {
	e := setup(t)
	cl, _, err := dial(t, "localhost", e.port, origin)
	if err != nil {
		t.Fatal(err)
	}
	cl.send("devices.list", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = cl.c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("erwartet Policy-Close, bekam %v", err)
	}
}

func get(t *testing.T, url, user, pass string, hdr map[string]string) (int, string) {
	req, _ := http.NewRequest("GET", url, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCompatWeightAndLegacy(t *testing.T) {
	e := setup(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", e.compat)
	if code, _ := get(t, base+"/weight", "", "", nil); code != 401 {
		t.Fatalf("ohne Auth: %d", code)
	}
	code, body := get(t, base+"/weight", "u", "p", nil)
	var w map[string]any
	json.Unmarshal([]byte(body), &w)
	if code != 200 || w["status"] != "success" || w["weight_kg"] != 1.56 || !strings.Contains(w["raw"].(string), "1.56kg") {
		t.Fatalf("/weight: %d %s", code, body)
	}
	if code, body := get(t, base+"/shot?preview=1", "u", "p", nil); code != 200 || body != "legacy /shot" {
		t.Fatalf("Legacy-Weiterleitung: %d %s", code, body)
	}
	e.fake.Silent(true)
	code, body = get(t, base+"/weight", "u", "p", nil)
	if code != 500 || !strings.Contains(body, `"status": "error"`) {
		t.Fatalf("stumme Waage: %d %s", code, body)
	}
}

func TestUIGuardsAndConfigUpdate(t *testing.T) {
	e := setup(t)
	base := fmt.Sprintf("http://localhost:%d", e.port)

	// DNS-Rebinding: fremder Host-Header auf unserem Port.
	if code, _ := get(t, base+"/api/config", "", "", map[string]string{"Host": fmt.Sprintf("evil.test:%d", e.port)}); code != 403 {
		t.Fatalf("fremder Host nicht abgelehnt: %d", code)
	}
	code, page := get(t, base+"/", "", "", nil)
	if code != 200 {
		t.Fatal(code)
	}
	csrf := regexp.MustCompile(`name="bridge-csrf" content="([0-9a-f]+)"`).FindStringSubmatch(page)[1]

	_, body := get(t, base+"/api/config", "", "", nil)
	if strings.Contains(body, `"password": "p"`) || !strings.Contains(body, `"********"`) {
		t.Fatalf("Passwort nicht maskiert: %s", body)
	}
	var cur struct{ Config config.Config }
	json.Unmarshal([]byte(body), &cur)

	put := func(c config.Config, hdr map[string]string) (int, string) {
		b, _ := json.Marshal(c)
		req, _ := http.NewRequest("PUT", base+"/api/config", strings.NewReader(string(b)))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(rb)
	}
	if code, _ := put(cur.Config, nil); code != 403 {
		t.Fatalf("ohne CSRF: %d", code)
	}
	if code, _ := put(cur.Config, map[string]string{"X-Bridge-CSRF": csrf, "Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Fatalf("cross-site: %d", code)
	}
	bad := cur.Config
	bad.AllowedOrigins = []string{"kein-origin"}
	if code, body := put(bad, map[string]string{"X-Bridge-CSRF": csrf}); code != 400 || !strings.Contains(body, "kein Origin") {
		t.Fatalf("ungültige Config: %d %s", code, body)
	}

	// Waage entfernen: Bridge läuft weiter, Desk bekommt device_missing,
	// /weight antwortet wie eine ausgesteckte Waage. Passwort bleibt erhalten.
	cl, _, err := dial(t, "localhost", e.port, origin)
	if err != nil {
		t.Fatal(err)
	}
	cl.hello()
	next := cur.Config
	next.Devices = nil
	if code, body := put(next, map[string]string{"X-Bridge-CSRF": csrf, "Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Fatalf("Speichern: %d %s", code, body)
	}
	if got := e.app.Config().HTTPCompat.BasicAuth.Password; got != "p" {
		t.Fatalf("Passwort überschrieben: %q", got)
	}
	if r := cl.call("scale.read", nil); r.OK || r.Error.Code != "device_missing" {
		t.Fatalf("nach Entfernen: %+v", r)
	}
	code, body = get(t, fmt.Sprintf("http://127.0.0.1:%d/weight", e.compat), "u", "p", nil)
	if code != 500 {
		t.Fatalf("/weight ohne Waage: %d %s", code, body)
	}
}
