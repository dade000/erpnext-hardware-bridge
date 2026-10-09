package metratec

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// NFC-Teil eines Dual-Frequenz-Tags (EM4425): Ein Handy liest den Speicher
// als NFC Forum Type 5 Tag. Damit es eine Adresse öffnet, muss dort stehen:
//
//	Capability Container (4 Byte)  E1 4x MLEN 00
//	NDEF-Message-TLV               03 <Länge> <NDEF-Message>
//	Terminator-TLV                 FE
//
// Die NDEF-Message ist ein einziger URI-Record (Short Record, TNF 1, Typ "U").
// Der Reader schreibt diese Bytes über UHF in den Nutzerspeicher; welcher
// Offset dort dem ersten NFC-Block entspricht, hängt von der Partitionierung
// des Chips ab und kommt deshalb vom Aufrufer.

// uriPrefixes sind die Abkürzungen aus der NFC-Forum-URI-Record-Spezifikation,
// soweit sie für Webadressen vorkommen. Längere zuerst, damit "https://www."
// vor "https://" greift.
var uriPrefixes = []struct {
	code   byte
	prefix string
}{
	{0x02, "https://www."},
	{0x01, "http://www."},
	{0x04, "https://"},
	{0x03, "http://"},
}

// blockSize ist die Blockgröße des NFC-Teils. Type 5 Tags haben fast immer
// 4 Byte; auf ganze Blöcke aufgefüllt wird nichts Halbes geschrieben.
const blockSize = 4

// EncodeType5URI baut den Speicherinhalt für einen URI-Record. readOnly setzt
// im Capability Container "Schreiben nie" – Handys und NFC-Apps behandeln den
// Tag dann als schreibgeschützt. Das ist eine Kennzeichnung, keine
// Hardware-Sperre.
func EncodeType5URI(uri string, readOnly bool) ([]byte, error) {
	if uri == "" {
		return nil, errors.New("leere Adresse")
	}
	code, rest := byte(0x00), uri
	for _, p := range uriPrefixes {
		if strings.HasPrefix(strings.ToLower(uri), p.prefix) {
			code, rest = p.code, uri[len(p.prefix):]
			break
		}
	}
	payload := append([]byte{code}, rest...)
	if len(payload) > 255 {
		return nil, fmt.Errorf("Adresse zu lang (%d Byte)", len(payload))
	}
	// MB=1 ME=1 SR=1 TNF=1 (NFC Forum well-known type)
	msg := append([]byte{0xD1, 0x01, byte(len(payload)), 'U'}, payload...)
	if len(msg) > 254 {
		return nil, fmt.Errorf("NDEF-Nachricht zu lang (%d Byte)", len(msg))
	}
	tlv := append([]byte{0x03, byte(len(msg))}, msg...)
	tlv = append(tlv, 0xFE)

	// MLEN = Größe des Datenbereichs in 8-Byte-Einheiten. Angegeben wird nur,
	// was geschrieben wird – die tatsächliche Größe des NFC-Speichers kennt
	// man ohne volles Datenblatt nicht, und ein Handy, das über das Ende
	// hinaus lesen will, bricht ab.
	mlen := (len(tlv) + 7) / 8
	if mlen > 255 {
		return nil, errors.New("NDEF-Nachricht zu lang für einen kurzen Capability Container")
	}
	access := byte(0x40) // Version 1.0, Lesen und Schreiben frei
	if readOnly {
		access |= 0x03 // Schreiben nie
	}
	out := append([]byte{0xE1, access, byte(mlen), 0x00}, tlv...)
	for len(out)%blockSize != 0 {
		out = append(out, 0x00)
	}
	return out, nil
}

// NDEFInfo ist das Ergebnis von DecodeType5.
type NDEFInfo struct {
	URI      string `json:"uri,omitempty"`
	ReadOnly bool   `json:"read_only"`
	// Offset ist die Stelle im gelesenen Speicher, an der der Capability
	// Container steht. Für die Einrichtung: wer die Adresse mit einer
	// Handy-App schreibt und dann über UHF liest, sieht hier, wo der
	// NFC-Bereich im UHF-Nutzerspeicher beginnt.
	Offset int `json:"offset"`
}

// FindType5 sucht einen Type-5-Capability-Container an jeder Blockgrenze
// und dekodiert den ersten URI-Record dahinter.
func FindType5(mem []byte) (*NDEFInfo, error) {
	var lastErr error = errors.New("kein NDEF-Inhalt gefunden")
	for off := 0; off+blockSize <= len(mem); off += 2 {
		if mem[off] != 0xE1 {
			continue
		}
		info, err := DecodeType5(mem[off:])
		if err == nil {
			info.Offset = off
			return info, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// DecodeType5 liest einen URI-Record ab Beginn eines Capability Containers.
func DecodeType5(mem []byte) (*NDEFInfo, error) {
	if len(mem) < 4 || mem[0] != 0xE1 {
		return nil, errors.New("kein Capability Container (E1) am Anfang")
	}
	info := &NDEFInfo{ReadOnly: mem[1]&0x03 == 0x03}
	i := 4
	if mem[2] == 0x00 { // 8-Byte-CC: MLEN steht in Byte 6–7
		i = 8
	}
	for i < len(mem) {
		t := mem[i]
		switch t {
		case 0x00: // NULL-TLV
			i++
			continue
		case 0xFE:
			return nil, errors.New("Terminator vor einer NDEF-Nachricht")
		}
		if i+1 >= len(mem) {
			break
		}
		l, hdr := int(mem[i+1]), 2
		if l == 0xFF {
			if i+3 >= len(mem) {
				break
			}
			l, hdr = int(mem[i+2])<<8|int(mem[i+3]), 4
		}
		body := mem[i+hdr:]
		if len(body) < l {
			return nil, errors.New("NDEF-Nachricht abgeschnitten (mehr Speicher lesen)")
		}
		if t != 0x03 {
			i += hdr + l
			continue
		}
		uri, err := decodeURIRecord(body[:l])
		if err != nil {
			return nil, err
		}
		info.URI = uri
		return info, nil
	}
	return nil, errors.New("keine NDEF-Nachricht gefunden")
}

func decodeURIRecord(msg []byte) (string, error) {
	if len(msg) < 3 {
		return "", errors.New("NDEF-Record zu kurz")
	}
	h := msg[0]
	if h&0x10 == 0 {
		return "", errors.New("nur kurze NDEF-Records werden gelesen")
	}
	typeLen := int(msg[1])
	payLen := int(msg[2])
	i := 3
	if h&0x08 != 0 { // ID-Länge vorhanden
		i++
	}
	if len(msg) < i+typeLen+payLen {
		return "", errors.New("NDEF-Record abgeschnitten")
	}
	idLen := 0
	if h&0x08 != 0 {
		idLen = int(msg[3])
	}
	typ := msg[i : i+typeLen]
	pay := msg[i+typeLen+idLen:]
	if len(pay) < payLen {
		return "", errors.New("NDEF-Record abgeschnitten")
	}
	pay = pay[:payLen]
	if h&0x07 != 0x01 || !bytes.Equal(typ, []byte("U")) || len(pay) == 0 {
		return "", fmt.Errorf("erster Record ist keine Adresse (TNF %d, Typ %q)", h&0x07, typ)
	}
	prefix := ""
	for _, p := range uriPrefixes {
		if p.code == pay[0] {
			prefix = p.prefix
		}
	}
	return prefix + string(pay[1:]), nil
}

// keepCC übernimmt aus einem vorhandenen Type-5-Capability-Container (4 Byte,
// Version 1.x) Größe und Merkmale, wenn der Bereich groß genug ist. Die
// Zugriffsbits (Schreibschutz-Kennzeichnung) bleiben die eigenen.
func keepCC(data, cc []byte) {
	if len(cc) < 4 || cc[0] != 0xE1 || cc[1]&0xF0 != 0x40 || cc[2] == 0 {
		return
	}
	if int(cc[2]) < int(data[2]) {
		return // vorhandener Bereich zu klein angegeben – eigene Angabe behalten
	}
	data[2], data[3] = cc[2], cc[3]
}
