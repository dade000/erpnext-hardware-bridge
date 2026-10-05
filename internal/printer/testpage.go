package printer

import (
	"bytes"
	"fmt"
	"time"
)

// testLabel ist ein kleines eigenes Etikett für den Testknopf. Es ist kein
// Carrier-Label – die werden nie von der Bridge erzeugt oder verändert.
func testLabel(id string) []byte {
	return []byte("^XA^CI28" +
		"^FO40,40^A0N,48,48^FDERPNext Hardware Bridge^FS" +
		"^FO40,110^A0N,34,34^FDTestdruck: " + id + "^FS" +
		"^FO40,160^A0N,28,28^FD" + time.Now().Format("02.01.2006 15:04:05") + "^FS" +
		"^XZ")
}

// testReceipt ist ein kurzer Testbon in ESC/POS: Drucker zurücksetzen, drei
// Zeilen (die erste fett), Vorschub, Schnitt. Nur ASCII, damit keine
// Zeichentabelle eine Rolle spielt.
func testReceipt(id string) []byte {
	var b bytes.Buffer
	b.WriteString("\x1b@")                                       // ESC @: zurücksetzen
	b.WriteString("\x1bE\x01ERPNext Hardware Bridge\n\x1bE\x00") // fett an/aus
	b.WriteString("Testdruck: " + id + "\n")
	b.WriteString(time.Now().Format("02.01.2006 15:04:05") + "\n")
	b.WriteString("\x1bd\x04")     // ESC d 4: vier Zeilen Vorschub
	b.WriteString("\x1dV\x42\x00") // GS V B 0: Teilschnitt
	return b.Bytes()
}

// testPDF ist die Testseite für Drucker, die PDF annehmen: eine A4-Seite mit
// drei Zeilen Text in einer Standardschrift.
func testPDF(id string) []byte {
	lines := []string{
		"ERPNext Hardware Bridge",
		"Testdruck: " + id,
		time.Now().Format("02.01.2006 15:04:05"),
	}
	var content bytes.Buffer
	content.WriteString("BT /F1 24 Tf 72 760 Td 0 -36 TL\n")
	for _, l := range lines {
		fmt.Fprintf(&content, "(%s) Tj T*\n", pdfEscape(l))
	}
	content.WriteString("ET\n")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// pdfEscape macht einen Text zu einem PDF-Literal; Zeichen außerhalb von
// ASCII fallen weg (die Standardschrift kennt sie ohne Kodierungsangabe nicht).
func pdfEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}
