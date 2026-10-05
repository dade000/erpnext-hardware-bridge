package printer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
)

// fakeIPP ist ein IPP-Drucker für Tests: Er beantwortet
// Get-Printer-Attributes mit seinen Formaten und merkt sich Druckaufträge.
type fakeIPP struct {
	mu        sync.Mutex
	formats   []string
	state     int32 // 3 idle, 5 stopped
	accepting bool
	reject    uint16 // Statuscode für Print-Job, 0 = annehmen
	jobs      []fakeIPPJob
}

type fakeIPPJob struct {
	format  string
	name    string
	media   string
	scaling string
	uri     string
	doc     []byte
}

func (f *fakeIPP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req, err := parseIPP(body)
	if err != nil || r.Header.Get("Content-Type") != "application/ipp" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var out bytes.Buffer
	status := uint16(0)
	if req.status == ippOpPrintJob && f.reject != 0 {
		status = f.reject
	}
	out.Write([]byte{2, 0})
	_ = binary.Write(&out, binary.BigEndian, status)
	out.Write(body[4:8]) // request-id
	writeAttr(&out, ippTagOperation, ippTagCharset, "attributes-charset", []byte("utf-8"))
	writeAttr(&out, 0, ippTagLanguage, "attributes-natural-language", []byte("de"))

	switch req.status {
	case ippOpGetPrinterAttributes:
		for i, m := range f.formats {
			group, name := byte(0), ""
			if i == 0 {
				group, name = 0x04, "document-format-supported"
			}
			writeAttr(&out, group, ippTagMime, name, []byte(m))
		}
		state := make([]byte, 4)
		binary.BigEndian.PutUint32(state, uint32(f.state))
		writeAttr(&out, 0, ippTagEnum, "printer-state", state)
		acc := []byte{0}
		if f.accepting {
			acc[0] = 1
		}
		writeAttr(&out, 0, ippTagBoolean, "printer-is-accepting-jobs", acc)
		writeAttr(&out, 0, ippTagKeyword, "printer-state-reasons", []byte("media-empty"))
	case ippOpPrintJob:
		if status == 0 {
			first := func(n string) string {
				if v := req.strings(n); len(v) > 0 {
					return v[0]
				}
				return ""
			}
			f.jobs = append(f.jobs, fakeIPPJob{
				format: first("document-format"), name: first("job-name"), media: first("media"),
				scaling: first("print-scaling"), uri: first("printer-uri"), doc: append([]byte(nil), req.rest...),
			})
			id := make([]byte, 4)
			binary.BigEndian.PutUint32(id, uint32(len(f.jobs)))
			writeAttr(&out, 0x02, ippTagInteger, "job-id", id)
		} else {
			writeAttr(&out, 0, 0x41, "status-message", []byte("client-error-document-format-not-supported"))
		}
	}
	out.WriteByte(ippTagEnd)
	w.Header().Set("Content-Type", "application/ipp")
	_, _ = w.Write(out.Bytes())
}

func writeAttr(out *bytes.Buffer, group, tag byte, name string, value []byte) {
	if group != 0 {
		out.WriteByte(group)
	}
	out.WriteByte(tag)
	_ = binary.Write(out, binary.BigEndian, uint16(len(name)))
	out.WriteString(name)
	_ = binary.Write(out, binary.BigEndian, uint16(len(value)))
	out.Write(value)
}

func newFakeIPP(t *testing.T, formats ...string) (*fakeIPP, config.DeviceConfig) {
	t.Helper()
	f := &fakeIPP{formats: formats, state: 3, accepting: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	uri := "ipp://" + strings.TrimPrefix(srv.URL, "http://") + "/ipp/print"
	return f, config.DeviceConfig{ID: "buero", Kind: Kind, Driver: "ipp", URI: uri}
}

func TestIPPReportsOnlyFormatsThePrinterNames(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		mimes []string
		want  string
	}{
		{[]string{"application/pdf", "image/pwg-raster", "image/jpeg"}, "pdf"},
		{[]string{"application/vnd.cups-raw", "application/octet-stream"}, "zpl"},
		{[]string{"application/pdf", "application/vnd.cups-raw"}, "pdf,zpl"},
	} {
		_, cfg := newFakeIPP(t, tc.mimes...)
		sink := newIPPSink(cfg)
		if err := sink.Check(ctx); err != nil {
			t.Fatalf("%v: %v", tc.mimes, err)
		}
		if got := strings.Join(sink.Formats(), ","); got != tc.want {
			t.Fatalf("%v: Formate %q, erwartet %q", tc.mimes, got, tc.want)
		}
	}
	// Ein CUPS-Server meldet für jede Warteschlange beides; die Konfiguration
	// legt fest, wofür der Drucker dahinter taugt.
	_, both := newFakeIPP(t, "application/pdf", "application/vnd.cups-raw")
	both.Accept = "pdf"
	limited := newIPPSink(both)
	if err := limited.Check(ctx); err != nil || strings.Join(limited.Formats(), ",") != "pdf" {
		t.Fatalf("accept=pdf: %v %v", err, limited.Formats())
	}
	// Eine Roh-Warteschlange vor einem Bondrucker: Rohdaten heißen ESC/POS.
	_, bon := newFakeIPP(t, "application/pdf", "application/vnd.cups-raw")
	bon.Accept = "escpos"
	bonSink := newIPPSink(bon)
	if err := bonSink.Check(ctx); err != nil || strings.Join(bonSink.Formats(), ",") != "escpos" {
		t.Fatalf("accept=escpos: %v %v", err, bonSink.Formats())
	}
	// PDF ist bei IPP Everywhere optional: nur Raster heißt »nicht nutzbar«.
	_, cfg := newFakeIPP(t, "image/pwg-raster", "image/jpeg")
	sink := newIPPSink(cfg)
	if err := sink.Check(ctx); err == nil || len(sink.Formats()) != 0 {
		t.Fatalf("Raster-Drucker darf kein Format anbieten: %v %v", err, sink.Formats())
	}
}

func TestIPPSendsThePDFUnchanged(t *testing.T) {
	f, cfg := newFakeIPP(t, "application/pdf")
	cfg.Media, cfg.PrintScaling = "iso_a6_105x148mm", "fit"
	sink := newIPPSink(cfg)
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0x00, 0x03, 0xff, '\n'}, 5000)...)

	if err := sink.Write(context.Background(), Job{Title: "SAL-ORD-2026-01224", Format: "pdf", Data: pdf}); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs) != 1 {
		t.Fatalf("%d Aufträge", len(f.jobs))
	}
	job := f.jobs[0]
	if !bytes.Equal(job.doc, pdf) {
		t.Fatalf("Dokument verändert: %d Bytes statt %d", len(job.doc), len(pdf))
	}
	if job.format != "application/pdf" || job.name != "SAL-ORD-2026-01224" || job.uri != cfg.URI {
		t.Fatalf("Auftrag %+v", job)
	}
	if job.media != "iso_a6_105x148mm" || job.scaling != "fit" {
		t.Fatalf("Papier/Skalierung nicht übergeben: %+v", job)
	}
}

func TestIPPRawJobCarriesNoPaperOptions(t *testing.T) {
	f, cfg := newFakeIPP(t, "application/vnd.cups-raw")
	cfg.Media = "iso_a6_105x148mm"
	sink := newIPPSink(cfg)
	if err := sink.Write(context.Background(), Job{Title: "x", Format: "zpl", Data: []byte("^XA^XZ")}); err != nil {
		t.Fatal(err)
	}
	if job := f.jobs[0]; job.format != "application/vnd.cups-raw" || job.media != "" || string(job.doc) != "^XA^XZ" {
		t.Fatalf("Auftrag %+v", job)
	}
}

func TestIPPErrorsAreReported(t *testing.T) {
	ctx := context.Background()
	f, cfg := newFakeIPP(t, "application/pdf")
	sink := newIPPSink(cfg)

	f.reject = 0x040a
	err := sink.Write(ctx, Job{Title: "x", Format: "pdf", Data: []byte("%PDF")})
	if err == nil || !strings.Contains(err.Error(), "document-format-not-supported") {
		t.Fatalf("abgelehnter Auftrag: %v", err)
	}
	f.state = 5
	if err := sink.Check(ctx); err == nil || !strings.Contains(err.Error(), "media-empty") {
		t.Fatalf("angehaltener Drucker: %v", err)
	}
	f.state, f.accepting = 3, false
	if err := sink.Check(ctx); err == nil {
		t.Fatal("Drucker ohne Annahme muss als nicht erreichbar gelten")
	}
	dead := newIPPSink(config.DeviceConfig{URI: "ipp://127.0.0.1:1/ipp/print"})
	if err := dead.Check(ctx); err == nil {
		t.Fatal("unerreichbarer Drucker")
	}
}

func TestIPPAddressMapping(t *testing.T) {
	for uri, want := range map[string]string{
		"ipp://drucker.lan/ipp/print":                 "http://drucker.lan:631/ipp/print",
		"ipps://cups.example.at:443/printers/Versand": "https://cups.example.at:443/printers/Versand",
		"ipps://drucker.lan/ipp/print":                "https://drucker.lan:631/ipp/print",
	} {
		if got := newIPPSink(config.DeviceConfig{URI: uri}).httpURL; got != want {
			t.Errorf("%s -> %s, erwartet %s", uri, got, want)
		}
	}
}

// Ein PDF-Drucker bekommt PDF, lehnt ZPL ab und druckt als Testseite ein PDF.
func TestPDFPrinterThroughTheDevice(t *testing.T) {
	f, cfg := newFakeIPP(t, "application/pdf")
	p := NewWithSink(cfg, device.NewBus(), discardLog(), newIPPSink(cfg))
	ctx := context.Background()

	// Vor der ersten Antwort des Druckers ist kein Format bekannt; der
	// Testknopf fragt deshalb selbst nach.
	if _, err := p.Handle(ctx, "test", nil); err != nil {
		t.Fatalf("Testseite: %v", err)
	}
	if len(f.jobs) != 1 || !bytes.HasPrefix(f.jobs[0].doc, []byte("%PDF-1.4")) || !bytes.Contains(f.jobs[0].doc, []byte("Testdruck: buero")) {
		t.Fatalf("Testseite ist kein PDF: %q", f.jobs)
	}
	if !bytes.HasSuffix(f.jobs[0].doc, []byte("%%EOF\n")) {
		t.Fatal("Testseite endet nicht mit dem PDF-Schlusszeichen")
	}

	pdf := []byte("%PDF-1.7 Beleg")
	if _, err := p.Handle(ctx, "print", printReq("pdf", pdf, "Beleg")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.jobs[1].doc, pdf) {
		t.Fatalf("PDF verändert: %q", f.jobs[1].doc)
	}
	_, err := p.Handle(ctx, "print", printReq("zpl", []byte("^XA^XZ"), "Label"))
	if code := errCode(t, err); code != "unsupported_format" {
		t.Fatalf("zpl an PDF-Drucker: %s", code)
	}
	if st := p.Status(); strings.Join(st.Formats, ",") != "pdf" {
		t.Fatalf("Status-Formate %v", st.Formats)
	}
}

func TestPickPrefersTheDefaultPrinterForTheFormat(t *testing.T) {
	statuses := []device.Status{
		{ID: "waage", Kind: "scale"},
		{ID: "zebra", Kind: Kind, Formats: []string{"zpl"}, Default: true},
		{ID: "flur", Kind: Kind, Formats: []string{"pdf"}},
		{ID: "buero", Kind: Kind, Formats: []string{"pdf"}, Default: true},
		{ID: "neu", Kind: Kind}, // noch keine Antwort vom Drucker
	}
	for format, want := range map[string]string{"pdf": "buero", "zpl": "zebra", "png": ""} {
		if got := Pick(statuses, format); got != want {
			t.Errorf("%s -> %q, erwartet %q", format, got, want)
		}
	}
	// Ohne Standarddrucker: der erste, der das Format annimmt.
	if got := Pick(statuses[:3], "pdf"); got != "flur" {
		t.Errorf("ohne Standard: %q", got)
	}
	if NormalizeFormat(" RAW ") != "zpl" || NormalizeFormat("PDF") != "pdf" {
		t.Error("NormalizeFormat")
	}
}

func TestPrintRequestBase64RoundTrip(t *testing.T) {
	// printReq (aus printer_test.go) kodiert wie das Desk.
	raw := printReq("pdf", []byte{0, 1, 2, 255}, "x")
	if !strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255})) {
		t.Fatal("Base64 fehlt im Auftrag")
	}
}
