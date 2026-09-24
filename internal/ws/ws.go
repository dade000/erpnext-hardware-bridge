// Package ws implementiert die WebSocket-API unter /ws (Protokoll in
// docs/KONZEPT.md, Abschnitt 7).
package ws

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/netutil"
	"erpnext-hardware-bridge/internal/version"
)

const (
	helloTimeout = 3 * time.Second
	pingInterval = 20 * time.Second
	readLimit    = 64 << 10
	outBuffer    = 256
)

// Server nimmt WebSocket-Verbindungen an.
type Server struct {
	cfg func() *config.Config
	mgr *device.Manager
	bus *device.Bus
	log *slog.Logger

	mu       sync.Mutex
	sessions map[*session]struct{}
	started  time.Time
}

// ClientInfo beschreibt eine Verbindung für die Statusseite.
type ClientInfo struct {
	Origin     string    `json:"origin"`
	Client     string    `json:"client"`
	Remote     string    `json:"remote"`
	Since      time.Time `json:"since"`
	Subscribed []string  `json:"subscribed"`
}

// NewServer erzeugt den Server. cfg liefert die jeweils aktuelle Konfiguration.
func NewServer(cfg func() *config.Config, mgr *device.Manager, bus *device.Bus, log *slog.Logger) *Server {
	return &Server{cfg: cfg, mgr: mgr, bus: bus, log: log, sessions: map[*session]struct{}{}, started: time.Now()}
}

// Clients listet die offenen Verbindungen.
func (s *Server) Clients() []ClientInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClientInfo, 0, len(s.sessions))
	for ss := range s.sessions {
		out = append(out, ss.info())
	}
	return out
}

// Info ist der Inhalt von bridge.info und dem hello-Ergebnis.
func (s *Server) Info() map[string]any {
	c := s.cfg()
	return map[string]any{
		"version":  version.Version,
		"protocol": version.Protocol,
		"station":  c.Station,
		"platform": runtime.GOOS + "/" + runtime.GOARCH,
		"uptime_s": int(time.Since(s.started).Seconds()),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !netutil.OriginAllowed(origin, s.cfg().AllowedOrigins) {
		s.log.Warn("WebSocket abgelehnt: Origin nicht erlaubt", "origin", origin, "remote", r.RemoteAddr)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	// Die Origin-Prüfung ist oben erledigt; die eingebaute Prüfung der
	// Bibliothek vergleicht nur Host gegen Origin und passt hier nicht.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(readLimit)
	ss := &session{
		srv:    s,
		conn:   conn,
		origin: origin,
		remote: r.RemoteAddr,
		since:  time.Now(),
		out:    make(chan []byte, outBuffer),
		subs:   map[string]device.Device{},
		log:    s.log.With("origin", origin),
	}
	s.mu.Lock()
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, ss)
		s.mu.Unlock()
	}()
	ss.run(r.Context())
}

// --- Nachrichten ---

type inMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Type   string          `json:"type"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type resMsg struct {
	ID     json.RawMessage `json:"id"`
	Type   string          `json:"type"`
	OK     bool            `json:"ok"`
	Result any             `json:"result,omitempty"`
	Error  *device.Error   `json:"error,omitempty"`
}

type eventMsg struct {
	Type   string `json:"type"`
	Event  string `json:"event"`
	Device string `json:"device,omitempty"`
	Data   any    `json:"data"`
}

type helloParams struct {
	Client   string `json:"client"`
	Protocol int    `json:"protocol"`
	Token    string `json:"token"`
}

type deviceParams struct {
	Device string `json:"device"`
}

// --- Session ---

type session struct {
	srv    *Server
	conn   *websocket.Conn
	origin string
	remote string
	since  time.Time
	log    *slog.Logger
	out    chan []byte

	mu     sync.Mutex
	client string
	subs   map[string]device.Device
}

func (ss *session) info() ClientInfo {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	subs := make([]string, 0, len(ss.subs))
	for id := range ss.subs {
		subs = append(subs, id)
	}
	return ClientInfo{Origin: ss.origin, Client: ss.client, Remote: ss.remote, Since: ss.since, Subscribed: subs}
}

func (ss *session) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer ss.conn.CloseNow()
	defer ss.releaseAll()

	go ss.writer(ctx, cancel)

	if err := ss.hello(ctx); err != nil {
		ss.log.Info("WebSocket ohne gültiges hello geschlossen", "err", err)
		ss.conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	ss.log.Info("Client verbunden", "client", ss.client, "remote", ss.remote)

	events, unsub := ss.srv.bus.Subscribe(outBuffer)
	defer unsub()
	go ss.forward(ctx, events)
	go ss.pinger(ctx)

	for {
		typ, data, err := ss.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
				ss.log.Debug("WebSocket-Lesefehler", "err", err)
			}
			ss.log.Info("Client getrennt", "client", ss.client)
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m inMsg
		if err := json.Unmarshal(data, &m); err != nil || m.Type != "req" || m.Method == "" {
			ss.send(resMsg{ID: m.ID, Type: "res", Error: device.Errf("bad_request", "Nachricht nicht verstanden")})
			continue
		}
		// Parallel bearbeiten: ein langsames scale.read blockiert kein devices.list.
		go func() {
			res, err := ss.dispatch(ctx, m.Method, m.Params)
			ss.reply(m.ID, res, err)
		}()
	}
}

func (ss *session) hello(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	typ, data, err := ss.conn.Read(hctx)
	if err != nil {
		return errors.New("kein hello innerhalb von 3 s")
	}
	var m inMsg
	if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Method != "hello" {
		return errors.New("erste Nachricht muss hello sein")
	}
	var p helloParams
	_ = json.Unmarshal(m.Params, &p)
	if p.Protocol != version.Protocol {
		ss.reply(m.ID, nil, device.Errf("protocol_mismatch", "Bridge spricht Protokoll "+itoa(version.Protocol)))
		return errors.New("falsche Protokollversion")
	}
	if tok := ss.srv.cfg().Token; tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(p.Token)) != 1 {
		ss.reply(m.ID, nil, device.Errf("unauthorized", "Token falsch oder fehlt"))
		return errors.New("Token falsch")
	}
	ss.mu.Lock()
	ss.client = strings.TrimSpace(p.Client)
	ss.mu.Unlock()
	res := ss.srv.Info()
	res["devices"] = ss.srv.mgr.Statuses()
	ss.reply(m.ID, res, nil)
	return nil
}

func (ss *session) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "devices.list":
		return ss.srv.mgr.Statuses(), nil
	case "bridge.info":
		return ss.srv.Info(), nil
	case "hello":
		return nil, device.Errf("bad_request", "hello nur einmal")
	}
	kind, cmd, ok := strings.Cut(method, ".")
	if !ok || kind != "scale" {
		return nil, device.Errf("unknown_method", method+" gibt es nicht")
	}
	var p deviceParams
	_ = json.Unmarshal(raw, &p)
	dev, derr := ss.srv.mgr.Lookup(kind, p.Device)
	if derr != nil {
		return nil, derr
	}
	switch cmd {
	case "subscribe":
		ss.subscribe(dev)
		st := dev.Status()
		return map[string]any{"device": st.ID, "state": st.State, "message": st.Message, "last": st.Last}, nil
	case "unsubscribe":
		ss.unsubscribe(dev.ID())
		return map[string]any{"device": dev.ID()}, nil
	}
	return dev.Handle(ctx, cmd, raw)
}

// subscribe ist idempotent. Wurde das Gerät inzwischen neu gestartet
// (Konfiguration geändert), wandert der Bedarf vom alten auf den neuen Treiber.
func (ss *session) subscribe(dev device.Device) {
	ss.mu.Lock()
	old, had := ss.subs[dev.ID()]
	ss.subs[dev.ID()] = dev
	ss.mu.Unlock()
	if had && old == dev {
		return
	}
	if had {
		if d, ok := old.(device.DemandTracker); ok {
			d.AddDemand(-1)
		}
	}
	if d, ok := dev.(device.DemandTracker); ok {
		d.AddDemand(1)
	}
}

func (ss *session) unsubscribe(id string) {
	ss.mu.Lock()
	dev, had := ss.subs[id]
	delete(ss.subs, id)
	ss.mu.Unlock()
	if had {
		if d, ok := dev.(device.DemandTracker); ok {
			d.AddDemand(-1)
		}
	}
}

func (ss *session) releaseAll() {
	ss.mu.Lock()
	ids := make([]string, 0, len(ss.subs))
	for id := range ss.subs {
		ids = append(ids, id)
	}
	ss.mu.Unlock()
	for _, id := range ids {
		ss.unsubscribe(id)
	}
}

// forward reicht Bus-Events weiter: Zustände immer, Messwerte nur für
// abonnierte Geräte.
func (ss *session) forward(ctx context.Context, events <-chan device.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Name != "device.state" {
				ss.mu.Lock()
				_, sub := ss.subs[ev.Device]
				ss.mu.Unlock()
				if !sub {
					continue
				}
			}
			ss.send(eventMsg{Type: "event", Event: ev.Name, Device: ev.Device, Data: ev.Data})
		}
	}
}

func (ss *session) reply(id json.RawMessage, res any, err error) {
	if err == nil {
		ss.send(resMsg{ID: id, Type: "res", OK: true, Result: res})
		return
	}
	var de *device.Error
	if !errors.As(err, &de) {
		de = device.Errf("unexpected", err.Error())
	}
	ss.send(resMsg{ID: id, Type: "res", Error: de})
}

// send stellt eine Nachricht in die Ausgangswarteschlange. Ist sie voll
// (Client liest nicht), geht die Nachricht verloren statt alles zu blockieren.
func (ss *session) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		ss.log.Error("JSON-Kodierung", "err", err)
		return
	}
	select {
	case ss.out <- b:
	default:
		ss.log.Warn("Ausgangspuffer voll, Nachricht verworfen", "client", ss.client)
	}
}

func (ss *session) writer(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ss.out:
			wctx, c := context.WithTimeout(ctx, 5*time.Second)
			err := ss.conn.Write(wctx, websocket.MessageText, b)
			c()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func (ss *session) pinger(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, c := context.WithTimeout(ctx, 10*time.Second)
			err := ss.conn.Ping(pctx)
			c()
			if err != nil && ctx.Err() == nil {
				ss.log.Info("Client antwortet nicht auf Ping", "client", ss.client)
				ss.conn.Close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
