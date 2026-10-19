package streamdeck

import (
	"encoding/binary"
	"errors"
	"strings"
)

// VendorID von Elgato.
const VendorID = 0x0fd9

// Model beschreibt eine Stream-Deck-Bauform. Alle unterstützten Modelle
// sprechen das Protokoll der zweiten Generation: JPEG-Tastenbilder in
// 1024-Byte-Reports, Tastenzustände ab Byte 4 des Input-Reports.
type Model struct {
	Name    string
	PID     uint16
	Keys    int
	Columns int
	// Pixel pro Taste (quadratisch). Das Bild steht auf dem Kopf: beide
	// Achsen werden vor dem Senden gespiegelt.
	Size int
}

// Models: nur die 15-Tasten-Geräte der zweiten Generation. Mini und das
// Original der ersten Generation (BMP, andere Reports) fehlen bewusst.
var Models = []Model{
	{Name: "Stream Deck Original V2", PID: 0x006d, Keys: 15, Columns: 5, Size: 72},
	{Name: "Stream Deck MK.2", PID: 0x0080, Keys: 15, Columns: 5, Size: 72},
	{Name: "Stream Deck MK.2 (Scissor)", PID: 0x00a5, Keys: 15, Columns: 5, Size: 72},
}

// ModelByPID liefert die Bauform oder false, wenn sie nicht unterstützt ist.
func ModelByPID(pid uint16) (Model, bool) {
	for _, m := range Models {
		if m.PID == pid {
			return m, true
		}
	}
	return Model{}, false
}

const (
	imageReportLen = 1024
	imageHeaderLen = 8
	featureLen     = 32
	inputReportLen = 512
	keyOffset      = 4
)

// imageReports zerlegt ein JPEG in die Output-Reports für eine Taste:
// 0x02 0x07 <taste> <letzte?> <länge LE16> <seite LE16> + Nutzlast, mit
// Nullen auf 1024 Byte aufgefüllt.
func imageReports(key int, jpeg []byte) [][]byte {
	const chunk = imageReportLen - imageHeaderLen
	var out [][]byte
	for page := 0; ; page++ {
		start := page * chunk
		end := min(start+chunk, len(jpeg))
		r := make([]byte, imageReportLen)
		r[0], r[1], r[2] = 0x02, 0x07, byte(key)
		if end == len(jpeg) {
			r[3] = 1
		}
		binary.LittleEndian.PutUint16(r[4:], uint16(end-start))
		binary.LittleEndian.PutUint16(r[6:], uint16(page))
		copy(r[imageHeaderLen:], jpeg[start:end])
		out = append(out, r)
		if end == len(jpeg) {
			return out
		}
	}
}

func feature(b ...byte) []byte {
	r := make([]byte, featureLen)
	copy(r, b)
	return r
}

// brightnessReport setzt die Helligkeit in Prozent.
func brightnessReport(pct int) []byte { return feature(0x03, 0x08, byte(max(0, min(pct, 100)))) }

// resetReport zeigt wieder das Elgato-Logo.
func resetReport() []byte { return feature(0x03, 0x02) }

var errShortReport = errors.New("Input-Report zu kurz")

// parseKeys liest die Tastenzustände aus einem Input-Report
// (0x01 0x00 <anzahl LE16> <zustand je taste>).
func parseKeys(report []byte, keys int) ([]bool, error) {
	if len(report) < keyOffset+keys || report[0] != 0x01 {
		return nil, errShortReport
	}
	out := make([]bool, keys)
	for i := range out {
		out[i] = report[keyOffset+i] != 0
	}
	return out, nil
}

// featureString liest eine Zeichenkette aus einem Feature-Report (Seriennummer
// ab Byte 2, Firmware ab Byte 6), bis zur ersten Null.
func featureString(b []byte, from int) string {
	if len(b) <= from {
		return ""
	}
	s := b[from:]
	if i := strings.IndexByte(string(s), 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(string(s))
}
