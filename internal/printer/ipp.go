package printer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"erpnext-hardware-bridge/internal/config"
)

// ippSink druckt über IPP/IPPS. Der Drucker (oder ein CUPS-Server) bekommt
// das Dokument unverändert zusammen mit seinem Format: ein PDF als
// application/pdf, Rohdaten als application/vnd.cups-raw. Gerendert wird am
// Drucker, nicht in der Bridge.
//
// Welche Formate ein Drucker annimmt, sagt er selbst
// (document-format-supported). PDF ist bei IPP Everywhere optional, deshalb
// wird nichts angenommen, was er nicht meldet.
type ippSink struct {
	printerURI string // ipp(s)://…, geht als printer-uri mit
	httpURL    string // http(s)://…, dorthin geht der POST
	media      string
	scaling    string
	accept     string // "" = was der Drucker meldet, sonst nur dieses Format
	client     *http.Client

	mu      sync.Mutex
	formats []string
	reqID   int32
}

const (
	mimePDF = "application/pdf"
	mimeRaw = "application/vnd.cups-raw"

	ippOpPrintJob             = 0x0002
	ippOpGetPrinterAttributes = 0x000B

	ippTagOperation = 0x01
	ippTagJob       = 0x02
	ippTagEnd       = 0x03
	ippTagInteger   = 0x21
	ippTagBoolean   = 0x22
	ippTagEnum      = 0x23
	ippTagNameWoL   = 0x42
	ippTagKeyword   = 0x44
	ippTagURI       = 0x45
	ippTagCharset   = 0x47
	ippTagLanguage  = 0x48
	ippTagMime      = 0x49

	ippPrinterStopped = 5
)

func newIPPSink(cfg config.DeviceConfig) *ippSink {
	s := &ippSink{printerURI: cfg.URI, media: cfg.Media, scaling: cfg.PrintScaling, accept: cfg.Accept}
	if u, err := url.Parse(cfg.URI); err == nil {
		h := *u
		h.Scheme = "http"
		if u.Scheme == "ipps" {
			h.Scheme = "https"
		}
		if u.Port() == "" {
			h.Host = u.Hostname() + ":631"
		}
		s.httpURL = h.String()
	}
	s.client = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.InsecureTLS}, //nolint:gosec // je Drucker einstellbar
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
	return s
}

func (s *ippSink) Target() string { return s.printerURI }

func (s *ippSink) Formats() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.formats...)
}

// Check fragt den Drucker nach Zustand und Formaten.
func (s *ippSink) Check(ctx context.Context) error {
	if s.httpURL == "" {
		return errors.New("keine gültige Adresse konfiguriert")
	}
	req := s.newRequest(ippOpGetPrinterAttributes)
	req.attr(ippTagKeyword, "requested-attributes",
		"document-format-supported", "printer-state", "printer-state-reasons", "printer-is-accepting-jobs")
	req.end()
	res, err := s.do(ctx, req.Bytes())
	if err != nil {
		return err
	}

	var formats []string
	for _, f := range res.strings("document-format-supported") {
		name := ""
		switch strings.ToLower(f) {
		case mimePDF:
			name = "pdf"
		case mimeRaw:
			// Rohdaten: Etikettendrucker (ZPL) oder Bondrucker (ESC/POS),
			// sehen kann man es nicht; accept entscheidet.
			name = "zpl"
			if s.accept == "escpos" {
				name = "escpos"
			}
		}
		if name != "" && (s.accept == "" || s.accept == name) {
			formats = append(formats, name)
		}
	}
	s.mu.Lock()
	s.formats = formats
	s.mu.Unlock()

	if v, ok := res.boolean("printer-is-accepting-jobs"); ok && !v {
		return errors.New("Drucker nimmt keine Aufträge an")
	}
	if st, ok := res.integer("printer-state"); ok && st == ippPrinterStopped {
		reasons := strings.Join(res.strings("printer-state-reasons"), ", ")
		return errors.New("Drucker ist angehalten (" + reasons + ")")
	}
	if len(formats) == 0 {
		if s.accept != "" {
			return errors.New("Drucker meldet " + strings.ToUpper(s.accept) + " nicht als Format")
		}
		return errors.New("Drucker meldet weder PDF noch Rohdaten als Format")
	}
	return nil
}

func (s *ippSink) Write(ctx context.Context, job Job) error {
	mime := mimeRaw
	if job.Format == "pdf" {
		mime = mimePDF
	}
	req := s.newRequest(ippOpPrintJob)
	req.attr(ippTagNameWoL, "requesting-user-name", "erpnext-hardware-bridge")
	req.attr(ippTagNameWoL, "job-name", job.Title)
	req.attr(ippTagMime, "document-format", mime)
	// Papier und Skalierung nur für Dokumente; Rohdaten bestimmen ihr Etikett selbst.
	if job.Format == "pdf" && (s.media != "" || s.scaling != "") {
		req.group(ippTagJob)
		if s.media != "" {
			req.attr(ippTagKeyword, "media", s.media)
		}
		if s.scaling != "" {
			req.attr(ippTagKeyword, "print-scaling", s.scaling)
		}
	}
	req.end()
	req.Write(job.Data)
	_, err := s.do(ctx, req.Bytes())
	return err
}

func (s *ippSink) do(ctx context.Context, body []byte) (*ippResponse, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.httpURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/ipp")
	resp, err := s.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Drucker verlangt eine Anmeldung (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d vom Drucker", resp.StatusCode)
	}
	res, err := parseIPP(raw)
	if err != nil {
		return nil, err
	}
	if res.status > 0x00ff {
		msg := strings.Join(res.strings("status-message"), " ")
		if msg == "" {
			msg = ippStatusText(res.status)
		}
		return nil, fmt.Errorf("Drucker lehnt ab: %s (IPP 0x%04x)", msg, res.status)
	}
	return res, nil
}

func ippStatusText(code uint16) string {
	switch code {
	case 0x0400:
		return "Anfrage nicht verstanden"
	case 0x0401, 0x0402, 0x0403:
		return "nicht berechtigt"
	case 0x0406:
		return "Drucker nicht gefunden"
	case 0x040a:
		return "Dokumentformat wird nicht unterstützt"
	case 0x0506:
		return "Drucker nimmt keine Aufträge an"
	case 0x0507:
		return "Drucker ist beschäftigt"
	}
	return "Fehler"
}

// --- IPP-Kodierung (RFC 8010), nur was die Bridge braucht ---

type ippRequest struct{ bytes.Buffer }

func (s *ippSink) newRequest(op uint16) *ippRequest {
	s.mu.Lock()
	s.reqID++
	id := s.reqID
	s.mu.Unlock()

	r := &ippRequest{}
	r.Write([]byte{2, 0}) // IPP 2.0
	_ = binary.Write(r, binary.BigEndian, op)
	_ = binary.Write(r, binary.BigEndian, id)
	r.group(ippTagOperation)
	// Diese drei müssen in genau dieser Reihenfolge am Anfang stehen.
	r.attr(ippTagCharset, "attributes-charset", "utf-8")
	r.attr(ippTagLanguage, "attributes-natural-language", "de")
	r.attr(ippTagURI, "printer-uri", s.printerURI)
	return r
}

func (r *ippRequest) group(tag byte) { r.WriteByte(tag) }
func (r *ippRequest) end()           { r.WriteByte(ippTagEnd) }

// attr schreibt ein Attribut; weitere Werte folgen mit leerem Namen.
func (r *ippRequest) attr(tag byte, name string, values ...string) {
	for i, v := range values {
		r.WriteByte(tag)
		n := name
		if i > 0 {
			n = ""
		}
		_ = binary.Write(r, binary.BigEndian, uint16(len(n)))
		r.WriteString(n)
		_ = binary.Write(r, binary.BigEndian, uint16(len(v)))
		r.WriteString(v)
	}
}

type ippValue struct {
	tag  byte
	data []byte
}

type ippResponse struct {
	status uint16 // bei einer Anfrage: die Operation
	attrs  map[string][]ippValue
	rest   []byte // was nach den Attributen folgt (bei Print-Job das Dokument)
}

func parseIPP(b []byte) (*ippResponse, error) {
	if len(b) < 8 {
		return nil, errors.New("Antwort des Druckers ist kein IPP")
	}
	res := &ippResponse{status: binary.BigEndian.Uint16(b[2:4]), attrs: map[string][]ippValue{}}
	p := b[8:]
	last := ""
	for len(p) > 0 {
		tag := p[0]
		p = p[1:]
		if tag == ippTagEnd {
			res.rest = p
			break
		}
		if tag < 0x10 { // Gruppenanfang
			last = ""
			continue
		}
		if len(p) < 2 {
			return nil, errors.New("IPP-Antwort abgeschnitten")
		}
		nlen := int(binary.BigEndian.Uint16(p))
		if len(p) < 2+nlen+2 {
			return nil, errors.New("IPP-Antwort abgeschnitten")
		}
		name := string(p[2 : 2+nlen])
		p = p[2+nlen:]
		vlen := int(binary.BigEndian.Uint16(p))
		if len(p) < 2+vlen {
			return nil, errors.New("IPP-Antwort abgeschnitten")
		}
		val := p[2 : 2+vlen]
		p = p[2+vlen:]
		if name == "" {
			name = last // weiterer Wert des vorigen Attributs
		} else {
			last = name
		}
		if name != "" {
			res.attrs[name] = append(res.attrs[name], ippValue{tag: tag, data: val})
		}
	}
	return res, nil
}

func (r *ippResponse) strings(name string) []string {
	var out []string
	for _, v := range r.attrs[name] {
		out = append(out, string(v.data))
	}
	return out
}

func (r *ippResponse) integer(name string) (int, bool) {
	for _, v := range r.attrs[name] {
		if (v.tag == ippTagInteger || v.tag == ippTagEnum) && len(v.data) == 4 {
			return int(int32(binary.BigEndian.Uint32(v.data))), true
		}
	}
	return 0, false
}

func (r *ippResponse) boolean(name string) (bool, bool) {
	for _, v := range r.attrs[name] {
		if v.tag == ippTagBoolean && len(v.data) == 1 {
			return v.data[0] != 0, true
		}
	}
	return false, false
}
