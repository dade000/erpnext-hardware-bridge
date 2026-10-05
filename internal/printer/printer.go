// Package printer ist das Druckmodul der Bridge: es reicht Druckdaten
// unverändert an einen Drucker am Stations-PC weiter.
//
// Das Desk holt das Dokument vom ERPNext-Server und übergibt es Byte für
// Byte, zusammen mit seinem Format. Die Bridge rendert nichts, wandelt nichts
// um und schreibt nichts um – ein Label kommt so am Drucker an, wie der
// Carrier es geliefert hat. Ein Drucker bekommt nur Formate, die er selbst
// versteht; alles andere lehnt die Bridge ab, statt es umzuwandeln.
//
// Vier Wege zum Drucker:
//
//	raw_tcp   Netzwerkdrucker, Port 9100 (JetDirect/RAW)                  zpl oder escpos
//	raw_file  Gerätedatei, z.B. /dev/usb/lp0                              zpl oder escpos
//	system    Druckwarteschlange des Betriebssystems, roh                 zpl oder escpos
//	ipp       IPP/IPPS-Drucker oder CUPS-Server; Formate meldet der       pdf, zpl/escpos
//	          Drucker selbst (PDF nur, wenn er es nativ annimmt)
//
// Rohdaten sind Rohdaten: ob dahinter ein Etikettendrucker (ZPL) oder ein
// Bondrucker (ESC/POS) steht, kann die Bridge nicht sehen. Das sagt accept
// in der Konfiguration; ohne Angabe gilt ZPL wie bisher.
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
	// Kilobyte, ein Beleg als PDF mit Bildern wenige Megabyte.
	MaxJobBytes = 16 << 20
)

// Job ist ein Druckauftrag.
type Job struct {
	Title  string
	Format string // "zpl", "escpos" oder "pdf"
	Data   []byte
}

// Sink ist ein Weg zum Drucker.
type Sink interface {
	// Target beschreibt das Ziel für Statusseite und Logs.
	Target() string
	// Formats nennt, was der Drucker annimmt ("zpl", "pdf"). Bei IPP steht
	// das erst nach dem ersten Check fest.
	Formats() []string
	// Check prüft, ob der Drucker erreichbar ist, ohne etwas zu drucken.
	Check(ctx context.Context) error
	// Write schickt die Daten unverändert an den Drucker.
	Write(ctx context.Context, job Job) error
}

// rawOnly ist die Formatliste der Treiber, die Bytes ungefiltert durchreichen.
type rawOnly struct{}

func (rawOnly) Formats() []string { return []string{"zpl"} }

// rawLanguage legt fest, welche Druckersprache hinter einem Rohdrucker steht.
type rawLanguage struct {
	Sink
	format string
}

func (r rawLanguage) Formats() []string { return []string{r.format} }

// rawSink setzt die Sprache aus accept, wenn sie nicht ZPL ist.
func rawSink(cfg config.DeviceConfig, s Sink) Sink {
	if cfg.Accept == "escpos" {
		return rawLanguage{Sink: s, format: "escpos"}
	}
	return s
}

// LastJob beschreibt den letzten Auftrag (Statusseite).
type LastJob struct {
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
	p := &Printer{
		cfg:  cfg,
		log:  log,
		st:   device.NewStatusHolder(bus, cfg.ID, Kind, cfg.Driver, sink.Target()),
		sink: sink,
		wake: make(chan struct{}, 1),
	}
	p.st.Update(func(st *device.Status) {
		st.Formats = sink.Formats()
		st.Default = cfg.Default
	})
	return p
}

func (p *Printer) ID() string   { return p.cfg.ID }
func (p *Printer) Kind() string { return Kind }

func (p *Printer) Status() device.Status {
	st := p.st.Get()
	st.Formats = p.sink.Formats() // bei IPP erst nach der ersten Antwort bekannt
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
	// Vor dem Zustandswechsel, damit device.state die Formate schon trägt.
	formats := p.sink.Formats()
	p.st.Update(func(st *device.Status) { st.Formats = formats })
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
		format := NormalizeFormat(in.Format)
		if !Accepts(p.sink.Formats(), format) {
			// Nichts wird umgewandelt: Ein PDF käme am Etikettendrucker als
			// Zeichensalat heraus, ZPL an einem Bürodrucker ebenso.
			return nil, device.Errf("unsupported_format",
				"Drucker "+p.cfg.ID+" nimmt "+strings.ToUpper(in.Format)+" nicht an (er kann: "+
					strings.ToUpper(strings.Join(p.sink.Formats(), ", "))+").")
		}
		data, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			return nil, device.Errf("bad_request", "Druckdaten sind kein gültiges Base64")
		}
		return p.print(ctx, format, in.Title, data)
	case "test", "read":
		// "read" ist das Kommando des Testknopfs der Oberfläche. Die
		// Testseite kommt in einer Sprache, die der Drucker versteht.
		formats := p.sink.Formats()
		if len(formats) == 0 {
			// IPP vor der ersten Antwort: jetzt fragen.
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			err := p.sink.Check(cctx)
			cancel()
			if formats = p.sink.Formats(); len(formats) == 0 {
				msg := "Drucker nennt kein Format"
				if err != nil {
					msg = err.Error()
				}
				return nil, device.Errf("print_failed", "Druck fehlgeschlagen: "+msg)
			}
		}
		if Accepts(formats, "zpl") {
			return p.print(ctx, "zpl", "Testdruck", testLabel(p.cfg.ID))
		}
		if Accepts(formats, "escpos") {
			return p.print(ctx, "escpos", "Testdruck", testReceipt(p.cfg.ID))
		}
		return p.print(ctx, "pdf", "Testdruck", testPDF(p.cfg.ID))
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
	err := p.sink.Write(jctx, Job{Title: title, Format: format, Data: data})
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

	job := LastJob{TS: time.Now(), Title: title, Format: format, Bytes: len(data)}
	p.statsMu.Lock()
	p.stats.Jobs++
	p.statsMu.Unlock()
	p.st.Update(func(st *device.Status) { st.Last = job })
	p.st.Set(device.StateOnline, "")
	p.log.Info("Gedruckt", "title", title, "format", format, "bytes", len(data))
	return map[string]any{"device": p.cfg.ID, "bytes": len(data), "title": title, "format": format}, nil
}

func (p *Printer) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// NormalizeFormat bringt die Formatangabe eines Auftrags in die Form der
// Formatlisten. "raw" ist der alte Name für Rohdaten in Druckersprache.
func NormalizeFormat(f string) string {
	f = strings.ToLower(strings.TrimSpace(f))
	switch f {
	case "raw":
		return "zpl"
	case "esc/pos", "esc_pos":
		return "escpos"
	}
	return f
}

// Accepts sagt, ob format in der Formatliste eines Druckers steht.
func Accepts(formats []string, format string) bool {
	for _, f := range formats {
		if f == format {
			return true
		}
	}
	return false
}

// Pick wählt den Drucker für einen Auftrag ohne Geräteangabe: den
// Standarddrucker des Arbeitsplatzes, wenn er das Format annimmt, sonst den
// ersten, der es annimmt. Leer, wenn es keinen gibt.
func Pick(statuses []device.Status, format string) string {
	first := ""
	for _, st := range statuses {
		if st.Kind != Kind || !Accepts(st.Formats, format) {
			continue
		}
		if st.Default {
			return st.ID
		}
		if first == "" {
			first = st.ID
		}
	}
	return first
}

// Register macht die Druckertreiber dem Manager bekannt.
func Register(m *device.Manager) {
	m.Register(Kind, "raw_tcp", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, rawSink(cfg, &tcpSink{addr: cfg.Address}))
	})
	m.Register(Kind, "raw_file", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, rawSink(cfg, &fileSink{path: cfg.Port.Path}))
	})
	m.Register(Kind, "system", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, rawSink(cfg, newSystemSink(cfg.Queue)))
	})
	m.Register(Kind, "ipp", func(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
		return NewWithSink(cfg, bus, log, newIPPSink(cfg))
	})
}
