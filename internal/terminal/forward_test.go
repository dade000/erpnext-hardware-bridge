package terminal

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

const origin = "https://erp.holzschuhe.at"

// fakeTerminal nimmt einen WebSocket-Handshake an, merkt sich den Host-Header
// und schickt danach jedes Byte zurück.
func fakeTerminal(t *testing.T) (addr string, hosts chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	hosts = make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				hosts <- req.Host
				io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				io.Copy(c, br)
			}()
		}
	}()
	return l.Addr().String(), hosts
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start lässt eine Weiterleitung auf einem freien Port laufen. target ist das
// Ziel, das Dial tatsächlich wählt (Loopback ist im Betrieb verboten, hier
// steht dort das Test-Terminal).
func start(t *testing.T, target string) (*Forward, int, context.CancelFunc) {
	t.Helper()
	port := freePort(t)
	cfg := config.DeviceConfig{ID: "terminal", Kind: Kind, Driver: "ws_forward", Address: "192.168.1.50:80", LocalPort: port}
	f := New(cfg, device.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)), func() []string { return []string{origin} })
	f.dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 50 && f.Status().State != device.StateOnline; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return f, port, cancel
}

func handshake(t *testing.T, port int, host, origin string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	req := "GET /SIXml HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: SIXml\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	io.WriteString(c, req+"\r\n")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	return c, br, status
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestForwardsHandshakeAndBytes(t *testing.T) {
	target, hosts := fakeTerminal(t)
	f, port, _ := start(t, target)

	c, br, status := handshake(t, port, "127.0.0.1:"+itoa(port), origin)
	if !strings.Contains(status, "101") {
		t.Fatalf("Handshake nicht durchgereicht: %q", status)
	}
	if h := <-hosts; h != "192.168.1.50:80" {
		t.Errorf("Host am Terminal %q, erwartet die Terminal-Adresse", h)
	}
	io.WriteString(c, "frame-bytes")
	buf := make([]byte, len("frame-bytes"))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "frame-bytes" {
		t.Fatalf("Bytes nicht durchgereicht: %q %v", buf, err)
	}
	if st := f.Status(); st.Stats.(map[string]any)["active"] != 1 {
		t.Errorf("aktive Verbindung nicht gezählt: %+v", st.Stats)
	}
}

func TestRefusesForeignOriginAndHost(t *testing.T) {
	target, hosts := fakeTerminal(t)
	_, port, _ := start(t, target)

	for name, tc := range map[string]struct{ host, origin string }{
		"fremder Origin": {"127.0.0.1:" + itoa(port), "https://evil.example"},
		"kein Origin":    {"127.0.0.1:" + itoa(port), ""},
		"DNS-Rebinding":  {"evil.example:" + itoa(port), origin},
	} {
		_, _, status := handshake(t, port, tc.host, tc.origin)
		if !strings.Contains(status, "403") {
			t.Errorf("%s: %q, erwartet 403", name, status)
		}
	}
	select {
	case h := <-hosts:
		t.Fatalf("abgelehnte Verbindung erreichte das Terminal (Host %q)", h)
	default:
	}
}

func TestUnreachableTerminal(t *testing.T) {
	f, port, _ := start(t, "127.0.0.1:"+itoa(freePort(t)))

	_, _, status := handshake(t, port, "localhost:"+itoa(port), origin)
	if !strings.Contains(status, "502") {
		t.Fatalf("%q, erwartet 502", status)
	}
	if st := f.Status(); st.State != device.StateOffline || !strings.Contains(st.Message, "nicht erreichbar") {
		t.Fatalf("Status %+v", st)
	}
	if _, err := f.Handle(context.Background(), "read", nil); err == nil {
		t.Fatal("Test meldet erreichbar")
	}
}

func TestStopClosesOpenConnections(t *testing.T) {
	target, _ := fakeTerminal(t)
	_, port, cancel := start(t, target)

	c, br, status := handshake(t, port, "[::1]:"+itoa(port), origin)
	if !strings.Contains(status, "101") {
		t.Fatalf("Handshake: %q", status)
	}
	cancel()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("Verbindung blieb nach dem Stoppen offen")
	}
}

func TestDialRefusesLoopbackTarget(t *testing.T) {
	cfg := config.DeviceConfig{ID: "t", Kind: Kind, Driver: "ws_forward", Address: "localhost:8735", LocalPort: 8736}
	f := New(cfg, device.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)), func() []string { return nil })
	if _, err := f.dialTarget(context.Background()); err == nil || !strings.Contains(err.Error(), "nicht erlaubt") {
		t.Fatalf("Loopback-Ziel nicht abgelehnt: %v", err)
	}
}

// Ein Terminal, das den Pfad nicht kennt, antwortet ohne 101. Die Kasse
// bekommt die Antwort unverändert, der Status sagt, was das Terminal meinte.
func TestTerminalRejectsHandshake(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
	}()
	f, port, _ := start(t, l.Addr().String())

	_, _, status := handshake(t, port, "127.0.0.1:"+itoa(port), origin)
	if !strings.Contains(status, "404") {
		t.Fatalf("Antwort des Terminals nicht weitergegeben: %q", status)
	}
	if st := f.Status(); st.State != device.StateOffline || !strings.Contains(st.Message, "404") {
		t.Fatalf("Status %+v", st)
	}
}

// Das Terminal soll den Handshake so sehen, wie der Browser ihn geschickt
// hat: Schreibweise und Reihenfolge der Kopfzeilen unverändert, nur Host neu.
func TestHandshakeIsForwardedByteExact(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		head, _ := readHead(bufio.NewReader(c))
		got <- string(head)
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: SIXml\r\n\r\n")
	}()
	_, port, _ := start(t, l.Addr().String())

	_, _, status := handshake(t, port, "127.0.0.1:"+itoa(port), origin)
	if !strings.Contains(status, "101") {
		t.Fatalf("Handshake: %q", status)
	}
	want := "GET /SIXml HTTP/1.1\r\nHost: 192.168.1.50:80\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: SIXml\r\n" +
		"Origin: " + origin + "\r\n\r\n"
	if h := <-got; h != want {
		t.Fatalf("beim Terminal angekommen:\n%q\nerwartet:\n%q", h, want)
	}
}

// Firefox öffnet vor dem eigentlichen Versuch eine leere Verbindung.
func TestEmptyConnectionIsNoRefusal(t *testing.T) {
	target, _ := fakeTerminal(t)
	f, port, _ := start(t, target)

	c, err := net.Dial("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	time.Sleep(100 * time.Millisecond)
	if n := f.Status().Stats.(map[string]any)["refused"]; n != 0 {
		t.Fatalf("leere Verbindung als Absage gezählt: %v", n)
	}
}
