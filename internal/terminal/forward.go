// Package terminal leitet die WebSocket-Verbindung der Kasse an das
// Zahlungsterminal weiter.
//
// Die Kasse läuft auf einer HTTPS-Seite. Von dort darf der Browser keine
// unverschlüsselte Verbindung ins LAN öffnen (Mixed Content), das Terminal
// (Worldline, SIXml über WebSocket) spricht aber nur ws://, kein wss://. Zu
// localhost darf der Browser dagegen auch von einer HTTPS-Seite ws:// öffnen.
// Die Bridge lauscht deshalb auf 127.0.0.1/::1 und reicht die Verbindung an
// das Terminal durch. Im POS Profile steht als Terminal-Adresse 127.0.0.1 mit
// dem lokalen Port.
//
// Abweichend von docs/KONZEPT.md Abschnitt 11 ist die Weiterleitung fest
// konfiguriert (bridge.yaml) statt zur Laufzeit per tunnel.open geöffnet; die
// Härtung aus Abschnitt 11.1 gilt trotzdem: Origin- und Host-Prüfung am
// Listener, Ziel nur im eigenen Netz, keine Inhalte im Log.
package terminal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/netutil"
)

// Kind ist die Geräteklasse.
const Kind = "terminal"

const (
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 3 * time.Second
	retryBind        = 5 * time.Second
)

// Register macht den Treiber bekannt. origins liefert die aktuell erlaubten
// Origins, damit eine Änderung in der Oberfläche ohne Neustart der
// Weiterleitung gilt.
func Register(m *device.Manager, origins func() []string) {
	m.Register(Kind, "ws_forward", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return New(cfg, bus, log, origins)
	})
}

// Forward ist eine laufende Weiterleitung.
type Forward struct {
	cfg     config.DeviceConfig
	log     *slog.Logger
	origins func() []string
	status  *device.StatusHolder

	mu       sync.Mutex
	active   int
	sessions int
	refused  int
	failures int
	lastErr  string
	lastSeen time.Time

	// dial ist austauschbar, damit Tests die Zielprüfung umgehen können.
	dial func(ctx context.Context) (net.Conn, error)
}

// New baut eine Weiterleitung, gestartet wird sie mit Run.
func New(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, origins func() []string) *Forward {
	f := &Forward{cfg: cfg, log: log, origins: origins}
	f.status = device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, f.portLabel())
	f.dial = f.dialTarget
	return f
}

func (f *Forward) portLabel() string {
	return fmt.Sprintf("localhost:%d → %s", f.cfg.LocalPort, f.cfg.Address)
}

func (f *Forward) ID() string   { return f.cfg.ID }
func (f *Forward) Kind() string { return Kind }

// Status liefert den Zustand samt Zählern.
func (f *Forward) Status() device.Status {
	st := f.status.Get()
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Stats = map[string]any{
		"active": f.active, "sessions": f.sessions, "refused": f.refused,
		"failures": f.failures, "last_error": f.lastErr,
	}
	if !f.lastSeen.IsZero() {
		st.Last = map[string]any{"ts": f.lastSeen}
	}
	return st
}

// Run bindet den lokalen Port und nimmt Verbindungen an, bis ctx endet.
// Ist der Port belegt, wird alle paar Sekunden neu versucht.
func (f *Forward) Run(ctx context.Context) {
	for {
		ls, warns, err := netutil.ListenLoopback(f.cfg.LocalPort)
		if err == nil {
			for _, w := range warns {
				f.log.Warn("Terminal-Weiterleitung nur teilweise gebunden", "warn", w)
			}
			f.status.Set(device.StateOnline, "wartet auf die Kasse")
			f.serve(ctx, ls)
			return
		}
		f.status.Set(device.StateError, fmt.Sprintf("Port %d nicht gebunden: %v", f.cfg.LocalPort, err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryBind):
		}
	}
}

func (f *Forward) serve(ctx context.Context, ls []netutil.Listener) {
	var conns sync.Map // offene Verbindungen, beim Stoppen schliessen
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			for {
				c, err := l.Accept()
				if err != nil {
					return // Listener geschlossen
				}
				conns.Store(c, struct{}{})
				// Kam die Verbindung herein, während serve schon aufräumt, hat
				// das Schliessen unten sie verpasst.
				if ctx.Err() != nil {
					c.Close()
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer conns.Delete(c)
					f.handle(ctx, c, &conns)
				}()
			}
		}(l)
	}
	<-ctx.Done()
	for _, l := range ls {
		l.Close()
	}
	conns.Range(func(k, _ any) bool {
		k.(net.Conn).Close()
		return true
	})
	wg.Wait()
}

// handle prüft den WebSocket-Handshake der Kasse, verbindet zum Terminal und
// reicht danach Bytes in beide Richtungen durch.
func (f *Forward) handle(ctx context.Context, c net.Conn, conns *sync.Map) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		f.refuse(c, http.StatusBadRequest, "kein HTTP-Handshake", err)
		return
	}
	origin := req.Header.Get("Origin")
	switch {
	case !netutil.IsLoopbackHost(req.Host, f.cfg.LocalPort):
		// Fremder Hostname auf 127.0.0.1: DNS-Rebinding.
		f.refuse(c, http.StatusForbidden, "Host nicht erlaubt", fmt.Errorf("host %q", req.Host))
		return
	case !netutil.OriginAllowed(origin, f.origins()):
		f.refuse(c, http.StatusForbidden, "Origin nicht erlaubt", fmt.Errorf("origin %q", origin))
		return
	case !strings.EqualFold(req.Header.Get("Upgrade"), "websocket"):
		f.refuse(c, http.StatusBadRequest, "kein WebSocket", nil)
		return
	}

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	t, err := f.dial(dctx)
	cancel()
	if err != nil {
		f.fail(err)
		f.refuse(c, http.StatusBadGateway, "Terminal nicht erreichbar", err)
		return
	}
	defer t.Close()
	conns.Store(t, struct{}{})
	defer conns.Delete(t)
	if ctx.Err() != nil {
		return
	}

	// Den Handshake unverändert weitergeben, nur Host zeigt aufs Terminal.
	if err := writeHandshake(t, req, f.cfg.Address); err != nil {
		f.fail(err)
		f.refuse(c, http.StatusBadGateway, "Terminal nicht erreichbar", err)
		return
	}
	// Die Antwort des Terminals auf den Handshake mitlesen und unverändert
	// weitergeben. Ins Log kommt nur die Statuszeile: daran sieht man, ob das
	// Terminal den WebSocket annimmt (101) oder etwa den Pfad nicht kennt.
	_ = t.SetReadDeadline(time.Now().Add(handshakeTimeout))
	tr := bufio.NewReader(t)
	head, status, proto, err := readResponseHead(tr)
	if err != nil {
		f.fail(fmt.Errorf("keine Antwort auf den Handshake: %w", err))
		f.refuse(c, http.StatusBadGateway, "Terminal antwortet nicht", err)
		return
	}
	if _, err := c.Write(head); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	_ = t.SetReadDeadline(time.Time{})
	if !strings.Contains(status, " 101 ") {
		f.log.Warn("Terminal nimmt den WebSocket nicht an", "antwort", status, "target", f.cfg.Address, "pfad", req.RequestURI)
		f.fail(fmt.Errorf("Terminal antwortet %q", status))
		return
	}

	f.begin()
	f.log.Info("Kasse mit Terminal verbunden", "origin", origin, "target", f.cfg.Address, "protokoll", proto)
	start := time.Now()
	pipe(c, br, t, tr)
	f.end()
	f.log.Info("Verbindung zum Terminal beendet", "dauer", time.Since(start).Round(time.Second).String())
}

func writeHandshake(w io.Writer, req *http.Request, host string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, req.RequestURI, host)
	if err := req.Header.Write(&b); err != nil {
		return err
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// readResponseHead liest die Antwort des Terminals bis zur Leerzeile und
// liefert sie roh zurück, dazu Statuszeile und Subprotokoll.
func readResponseHead(r *bufio.Reader) (head []byte, status, proto string, err error) {
	for {
		line, err := r.ReadString('\n')
		head = append(head, line...)
		if err != nil {
			return head, status, proto, err
		}
		t := strings.TrimRight(line, "\r\n")
		switch {
		case status == "":
			status = t
		case t == "":
			return head, status, proto, nil
		case strings.HasPrefix(strings.ToLower(t), "sec-websocket-protocol:"):
			proto = strings.TrimSpace(t[len("sec-websocket-protocol:"):])
		}
		if len(head) > 16<<10 {
			return head, status, proto, errors.New("Antwortkopf zu lang")
		}
	}
}

// pipe kopiert in beide Richtungen, bis eine Seite schliesst. Inhalte werden
// weder gelesen noch protokolliert (Zahlungsdaten). Gelesen wird über die
// Puffer des Handshakes, damit dort schon liegende Bytes nicht verloren gehen.
func pipe(a net.Conn, ar io.Reader, b net.Conn, br io.Reader) {
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, br)
	go cp(b, ar)
	<-done
	// Eine Seite ist zu: die andere auch schliessen, damit die zweite Kopie endet.
	a.Close()
	b.Close()
	<-done
}

// dialTarget löst das Ziel auf, prüft jede Adresse und verbindet zur geprüften
// (kein zweites Auflösen, sonst DNS-Rebinding).
func (f *Forward) dialTarget(ctx context.Context) (net.Conn, error) {
	host, port, err := net.SplitHostPort(f.cfg.Address)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s: keine Adresse", host)
	}
	for _, ip := range ips {
		if err := netutil.ForwardTargetAllowed(ip); err != nil {
			return nil, fmt.Errorf("Ziel %s nicht erlaubt: %w", f.cfg.Address, err)
		}
	}
	var d net.Dialer
	var last error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (f *Forward) refuse(c net.Conn, code int, why string, err error) {
	f.mu.Lock()
	f.refused++
	f.mu.Unlock()
	args := []any{"grund", why, "remote", c.RemoteAddr().String()}
	if err != nil {
		args = append(args, "err", err)
	}
	f.log.Warn("Verbindung zum Terminal abgelehnt", args...)
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(why), why)
}

func (f *Forward) fail(err error) {
	f.mu.Lock()
	f.failures++
	f.lastErr = err.Error()
	f.mu.Unlock()
	f.status.Set(device.StateOffline, "Terminal nicht erreichbar: "+err.Error())
}

func (f *Forward) begin() {
	f.mu.Lock()
	f.active++
	f.sessions++
	f.lastSeen = time.Now()
	f.mu.Unlock()
	f.status.Set(device.StateOnline, "Kasse verbunden")
}

func (f *Forward) end() {
	f.mu.Lock()
	f.active--
	n := f.active
	f.lastSeen = time.Now()
	f.mu.Unlock()
	if n == 0 {
		f.status.Set(device.StateOnline, "wartet auf die Kasse")
	}
}

// Handle beantwortet den Testknopf der Oberfläche: ist das Terminal
// erreichbar? Es wird nur eine TCP-Verbindung auf- und gleich wieder
// abgebaut, kein Handshake.
func (f *Forward) Handle(ctx context.Context, cmd string, _ json.RawMessage) (any, error) {
	switch cmd {
	case "test", "read":
		start := time.Now()
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := f.dial(dctx)
		cancel()
		if err != nil {
			f.fail(err)
			return nil, device.Errf("unreachable", "Terminal "+f.cfg.Address+" nicht erreichbar: "+err.Error())
		}
		c.Close()
		if f.status.Get().State == device.StateOffline {
			f.status.Set(device.StateOnline, "wartet auf die Kasse")
		}
		return map[string]any{
			"target": f.cfg.Address, "local_port": f.cfg.LocalPort,
			"ms": time.Since(start).Milliseconds(),
		}, nil
	}
	return nil, device.Errf("unknown_command", "Kommando "+cmd+" kennt die Terminal-Weiterleitung nicht")
}

var _ device.Device = (*Forward)(nil)
