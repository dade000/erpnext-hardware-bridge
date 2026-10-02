// Package printer ist das Druckmodul der Bridge: es reicht Druckdaten
// unverändert an einen Drucker am Stations-PC weiter.
//
// Gedacht für Etikettendrucker, die ihre Druckersprache selbst verstehen
// (ZPL): Das Desk holt das Label vom ERPNext-Server und übergibt es Byte für
// Byte. Die Bridge rendert nichts, wandelt nichts um und schreibt nichts im
// Label um – ein Label kommt so am Drucker an, wie der Carrier es geliefert
// hat. Formate, die ein Drucker nicht selbst versteht (PDF), lehnt sie ab;
// die druckt das Desk über den Dialog des Browsers.
//
// Drei Wege zum Drucker:
//
//	raw_tcp   Netzwerkdrucker, Port 9100 (JetDirect/RAW)
//	raw_file  Gerätedatei, z.B. /dev/usb/lp0
//	system    Druckwarteschlange des Betriebssystems, roh (CUPS bzw. Windows-Spooler)
package printer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

const (
	// Kind ist die Geräteklasse.
	Kind = "printer"

	checkInterval = 15 * time.Second
	checkTimeout  = 4 * time.Second
	jobTimeout    = 20 * time.Second
	// MaxJobBytes begrenzt einen Auftrag. Ein Versandlabel hat einige zehn
	// Kilobyte, mit eingebetteten Grafiken wenige hundert.
	MaxJobBytes = 4 << 20
)

// Sink ist ein Weg zum Drucker.
type Sink interface {
	// Target beschreibt das Ziel für Statusseite und Logs.
	Target() string
	// Check prüft, ob der Drucker erreichbar ist, ohne etwas zu drucken.
	Check(ctx context.Context) error
	// Write schickt die Daten unverändert an den Drucker.
	Write(ctx context.Context, title string, data []byte) error
}

// Job beschreibt den letzten Auftrag (Statusseite).
type Job struct {
	TS     time.Time `json:"ts"`
	Title  string    `json:"title,omitempty"`
	Format string    `json:"format"`
	Bytes  int       `json:"bytes"`
}

// Stats zählt für die Statusseite.
type Stats struct {
	Jobs      uint64 `json:"jobs"`
	Failures  uint64 `json:"failures"`
	LastError string `json:"last_error,omitempty"`
}

// Printer ist ein laufender Druckertreiber.
type Printer struct {
	cfg  config.DeviceConfig
	log  *slog.Logger
	st   *device.StatusHolder
	sink Sink
	wake chan struct{}

	jobMu   sync.Mutex // ein Auftrag nach dem anderen
	statsMu sync.Mutex
	stats   Stats
}

// NewWithSink baut einen Drucker über einem beliebigen Sink (Tests, Treiber).
func NewWithSink(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger, sink Sink) *Printer {
	return &Printer{
		cfg:  cfg,
		log:  log,
		st:   device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, sink.Target()),
		sink: sink,
		wake: make(chan struct{}, 1),
	}
}

func (p *Printer) ID() string   { return p.cfg.ID }
func (p *Printer) Kind() string { return Kind }

func (p *Printer) Status() device.Status {
	st := p.st.Get()
	p.statsMu.Lock()
	st.Stats = p.stats
	p.statsMu.Unlock()
	return st
}

// Run prüft regelmäßig die Erreichbarkeit, bis ctx endet. Gedruckt wird in
// Handle; ein Auftrag stößt danach eine sofortige Prüfung an.
func (p *Printer) Run(ctx context.Context) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		p.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

func (p *Printer) check(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	// Nicht mitten in einen laufenden Auftrag hinein prüfen.
	p.jobMu.Lock()
	err := p.sink.Check(cctx)
	p.jobMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		p.st.Set(device.StateOffline, "Drucker nicht erreichbar: "+err.Error())
		return
	}
	p.st.Set(device.StateOnline, "")
}

type printParams struct {
	Format string `json:"format"`
	Data   string `json:"data"` // base64
	Title  string `json:"title"`
}

// Handle führt print oder test aus.
func (p *Printer) Handle(ctx context.Context, cmd string, raw json.RawMessage) (any, error) {
	switch cmd {
	case "print":
		var in printParams
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, device.Errf("bad_request", "Druckauftrag nicht verstanden")
		}
		format := strings.ToLower(strings.TrimSpace(in.Format))
		if format != "zpl" && format != "raw" {
			// Die Bridge gibt Daten roh weiter. Ein PDF käme am
			// Etikettendrucker als Zeichensalat heraus.
			return nil, device.Errf("unsupported_format",
				"Dieser Drucker nimmt nur Rohdaten (ZPL). Format "+in.Format+" bitte über den Druckdialog drucken.")
		}
		data, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			return nil, device.Errf("bad_request", "Druckdaten sind kein gültiges Base64")
		}
		return p.print(ctx, format, in.Title, data)
	case "test", "read":
		// "read" ist das Kommando des Testknopfs der Oberfläche.
		return p.print(ctx, "zpl", "Testdruck", testLabel(p.cfg.ID))
	}
	return nil, device.Errf("unknown_method", "printer."+cmd+" gibt es nicht")
}

func (p *Printer) print(ctx context.Context, format, title string, data []byte) (any, error) {
	if len(data) == 0 {
		return nil, device.Errf("bad_request", "Druckauftrag ist leer")
	}
	if len(data) > MaxJobBytes {
		return nil, device.Errf("too_large", "Druckauftrag ist zu groß")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "ERPNext"
	}

	p.jobMu.Lock()
	jctx, cancel := context.WithTimeout(ctx, jobTimeout)
	err := p.sink.Write(jctx, title, data)
	cancel()
	p.jobMu.Unlock()

	if err != nil {
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "Zeitüberschreitung beim Senden"
		}
		p.statsMu.Lock()
		p.stats.Failures++
		p.stats.LastError = msg
		p.statsMu.Unlock()
		p.st.Set(device.StateOffline, "Druck fehlgeschlagen: "+msg)
		p.log.Error("Druck fehlgeschlagen", "title", title, "bytes", len(data), "err", err)
		p.poke()
		return nil, device.Errf("print_failed", "Druck fehlgeschlagen: "+msg)
	}

	job := Job{TS: time.Now(), Title: title, Format: format, Bytes: len(data)}
	p.statsMu.Lock()
	p.stats.Jobs++
	p.statsMu.Unlock()
	p.st.Update(func(st *device.Status) { st.Last = job })
	p.st.Set(device.StateOnline, "")
	p.log.Info("Gedruckt", "title", title, "format", format, "bytes", len(data))
	return map[string]any{"device": p.cfg.ID, "bytes": len(data), "title": title}, nil
}

func (p *Printer) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// testLabel ist ein kleines eigenes Etikett für den Testknopf. Es ist kein
// Carrier-Label – die werden nie von der Bridge erzeugt oder verändert.
func testLabel(id string) []byte {
	return []byte("^XA^CI28" +
		"^FO40,40^A0N,48,48^FDERPNext Hardware Bridge^FS" +
		"^FO40,110^A0N,34,34^FDTestdruck: " + id + "^FS" +
		"^FO40,160^A0N,28,28^FD" + time.Now().Format("02.01.2006 15:04:05") + "^FS" +
		"^XZ")
}

// Register macht die Druckertreiber dem Manager bekannt.
func Register(m *device.Manager) {
	m.Register(Kind, "raw_tcp", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, &tcpSink{addr: cfg.Address})
	})
	m.Register(Kind, "raw_file", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, &fileSink{path: cfg.Port.Path})
	})
	m.Register(Kind, "system", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, newSystemSink(cfg.Queue))
	})
}
