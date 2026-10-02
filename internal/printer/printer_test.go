package printer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// fakeSink merkt sich, was am »Drucker« ankommt.
type fakeSink struct {
	mu       sync.Mutex
	jobs     [][]byte
	titles   []string
	sent     []string // Format je Auftrag
	formats  []string // leer = wie ein Rohdrucker
	checkErr error
	writeErr error
}

func (f *fakeSink) Target() string { return "fake" }
func (f *fakeSink) Formats() []string {
	if f.formats == nil {
		return []string{"zpl"}
	}
	return f.formats
}
func (f *fakeSink) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkErr
}
func (f *fakeSink) Write(_ context.Context, job Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.jobs = append(f.jobs, append([]byte(nil), job.Data...))
	f.titles = append(f.titles, job.Title)
	f.sent = append(f.sent, job.Format)
	return nil
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestPrinter(sink Sink) *Printer {
	cfg := config.DeviceConfig{ID: "labeldrucker", Kind: Kind, Driver: "raw_tcp"}
	return NewWithSink(cfg, device.NewBus(), discardLog(), sink)
}

func printReq(format string, data []byte, title string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{
		"format": format,
		"data":   base64.StdEncoding.EncodeToString(data),
		"title":  title,
	})
	return b
}

func errCode(t *testing.T, err error) string {
	t.Helper()
	var de *device.Error
	if !errors.As(err, &de) {
		t.Fatalf("erwartet device.Error, bekommen %v", err)
	}
	return de.Code
}

// Der Kern des Moduls: Das Label kommt Byte für Byte so am Drucker an, wie
// das Desk es übergeben hat.
func TestPrintPassesBytesThroughUnchanged(t *testing.T) {
	sink := &fakeSink{}
	p := newTestPrinter(sink)
	// Umlaute, Steuerzeichen und ein »0,00 kg«-Feld: nichts davon darf die
	// Bridge anfassen.
	zpl := []byte("~JO\n^XA^CI28^FO10,10^FDÖsterreich 0,00 kg^FS\r\n^XZ\x00\xff")

	res, err := p.Handle(context.Background(), "print", printReq("zpl", zpl, "SHIPMENT-00150"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 || !bytes.Equal(sink.jobs[0], zpl) {
		t.Fatalf("Druckdaten verändert: %q", sink.jobs)
	}
	if sink.titles[0] != "SHIPMENT-00150" {
		t.Fatalf("Titel %q", sink.titles[0])
	}
	if got := res.(map[string]any)["bytes"]; got != len(zpl) {
		t.Fatalf("bytes = %v", got)
	}
	st := p.Status()
	if st.State != device.StateOnline || st.Stats.(Stats).Jobs != 1 {
		t.Fatalf("Status nach Druck: %+v", st)
	}
	if job := st.Last.(LastJob); job.Bytes != len(zpl) || job.Format != "zpl" {
		t.Fatalf("Last = %+v", job)
	}
}

func TestPrintRejectsWhatARawPrinterCannotPrint(t *testing.T) {
	sink := &fakeSink{}
	p := newTestPrinter(sink)
	ctx := context.Background()

	// PDF käme am Etikettendrucker als Zeichensalat heraus.
	_, err := p.Handle(ctx, "print", printReq("pdf", []byte("%PDF-1.5"), "x"))
	if code := errCode(t, err); code != "unsupported_format" {
		t.Fatalf("pdf: %s", code)
	}
	_, err = p.Handle(ctx, "print", printReq("zpl", nil, "x"))
	if code := errCode(t, err); code != "bad_request" {
		t.Fatalf("leer: %s", code)
	}
	_, err = p.Handle(ctx, "print", json.RawMessage(`{"format":"zpl","data":"kein base64!"}`))
	if code := errCode(t, err); code != "bad_request" {
		t.Fatalf("base64: %s", code)
	}
	_, err = p.Handle(ctx, "print", printReq("zpl", make([]byte, MaxJobBytes+1), "x"))
	if code := errCode(t, err); code != "too_large" {
		t.Fatalf("zu groß: %s", code)
	}
	_, err = p.Handle(ctx, "eject", nil)
	if code := errCode(t, err); code != "unknown_method" {
		t.Fatalf("unbekannt: %s", code)
	}
	if len(sink.jobs) != 0 {
		t.Fatalf("abgelehnte Aufträge wurden gedruckt: %d", len(sink.jobs))
	}
}

func TestFailedPrintIsReportedAndCounted(t *testing.T) {
	sink := &fakeSink{writeErr: errors.New("connection refused")}
	p := newTestPrinter(sink)

	_, err := p.Handle(context.Background(), "print", printReq("zpl", []byte("^XA^XZ"), "x"))
	if code := errCode(t, err); code != "print_failed" {
		t.Fatalf("code %s", code)
	}
	st := p.Status()
	if st.State != device.StateOffline {
		t.Fatalf("Zustand %s", st.State)
	}
	if stats := st.Stats.(Stats); stats.Failures != 1 || stats.Jobs != 0 || stats.LastError == "" {
		t.Fatalf("Stats %+v", stats)
	}
}

func TestRunReportsReachability(t *testing.T) {
	sink := &fakeSink{checkErr: errors.New("no route to host")}
	p := newTestPrinter(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	waitState(t, p, device.StateOffline)
	sink.mu.Lock()
	sink.checkErr = nil
	sink.mu.Unlock()
	p.poke()
	waitState(t, p, device.StateOnline)
}

func waitState(t *testing.T, p *Printer, want device.State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Zustand %s nicht erreicht (ist %s)", want, p.Status().State)
}

// raw_tcp gegen einen echten Listener: so spricht die Bridge mit einem
// Netzwerkdrucker auf Port 9100.
func TestTCPSinkDeliversTheJob(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan []byte, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			if len(b) > 0 {
				received <- b
			}
		}
	}()

	sink := &tcpSink{addr: ln.Addr().String()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sink.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	zpl := []byte("^XA^FO20,20^FDTest^FS^XZ")
	if err := sink.Write(ctx, Job{Title: "t", Format: "zpl", Data: zpl}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, zpl) {
			t.Fatalf("angekommen %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nichts angekommen")
	}

	ln.Close()
	if err := (&tcpSink{addr: ln.Addr().String()}).Check(ctx); err == nil {
		t.Fatal("Check gegen geschlossenen Port muss scheitern")
	}
}

func TestFileSinkWritesToTheDevice(t *testing.T) {
	path := t.TempDir() + "/lp0"
	sink := &fileSink{path: path}
	if err := sink.Check(context.Background()); err == nil {
		t.Fatal("fehlendes Gerät muss als nicht erreichbar gelten")
	}
	if err := writeEmpty(path); err != nil {
		t.Fatal(err)
	}
	if err := sink.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := sink.Write(context.Background(), Job{Title: "t", Format: "zpl", Data: []byte("^XA^XZ")}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readAll(t, path); got != "^XA^XZ" {
		t.Fatalf("Datei enthält %q", got)
	}
}

func TestTestLabelIsOwnLabelNotACarrierLabel(t *testing.T) {
	sink := &fakeSink{}
	p := newTestPrinter(sink)
	if _, err := p.Handle(context.Background(), "test", nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 || !bytes.Contains(sink.jobs[0], []byte("Testdruck: labeldrucker")) {
		t.Fatalf("Testetikett: %q", sink.jobs)
	}
}
