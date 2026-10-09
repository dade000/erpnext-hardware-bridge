package metratec

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestEncodeType5URIBytes(t *testing.T) {
	got, err := EncodeType5URI("https://holzschuhe.at/u/E2801191A5030060A1B2C3D4", true)
	if err != nil {
		t.Fatal(err)
	}
	rest := "holzschuhe.at/u/E2801191A5030060A1B2C3D4" // 40 Zeichen
	// Payload = Präfixcode + Rest = 41, Record = 4 + 41 = 45, TLV = 2 + 45 + 1 = 48
	want := "E1" + "43" + "06" + "00" + // CC: 48/8 = 6
		"03" + "2D" + // NDEF-TLV, 45 Byte
		"D1" + "01" + "29" + "55" + "04" + strings.ToUpper(hex.EncodeToString([]byte(rest))) +
		"FE"
	for len(want)%8 != 0 {
		want += "00"
	}
	if h := strings.ToUpper(hex.EncodeToString(got)); h != want {
		t.Fatalf("Bytes\n got %s\nwant %s", h, want)
	}
	if len(got)%4 != 0 {
		t.Fatalf("nicht auf Blöcke aufgefüllt: %d", len(got))
	}
}

func TestEncodeWritable(t *testing.T) {
	got, _ := EncodeType5URI("http://example.com", false)
	if got[1] != 0x40 {
		t.Fatalf("Zugriffsbyte %02X, erwartet 40", got[1])
	}
	if got[10] != 0x03 { // "http://" ohne www (CC 4, TLV 2, Record-Kopf 4)
		t.Fatalf("Präfixcode %02X, erwartet 03", got[10])
	}
}

func TestRoundTripAndFind(t *testing.T) {
	uri := "https://www.holzschuhe.at/u/ABC"
	data, err := EncodeType5URI(uri, true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := DecodeType5(data)
	if err != nil || info.URI != uri || !info.ReadOnly {
		t.Fatalf("Decode: %+v %v", info, err)
	}
	// Mit vorgelagertem Speicher (z.B. NFC-Bereich ab Byte 8).
	mem := append(make([]byte, 8), data...)
	info, err = FindType5(mem)
	if err != nil || info.Offset != 8 || info.URI != uri {
		t.Fatalf("Find: %+v %v", info, err)
	}
}

func TestFindNothing(t *testing.T) {
	if _, err := FindType5(make([]byte, 32)); err == nil {
		t.Fatal("leerer Speicher sollte keinen Treffer liefern")
	}
}

func TestDecodeTruncated(t *testing.T) {
	data, _ := EncodeType5URI("https://holzschuhe.at/u/0123456789ABCDEF01234567", false)
	if _, err := DecodeType5(data[:16]); err == nil || !strings.Contains(err.Error(), "abgeschnitten") {
		t.Fatalf("abgeschnittener Inhalt: %v", err)
	}
}

func TestParseInventory(t *testing.T) {
	tags, err := parseInventory([]string{
		"+INV: 3034257BF468D480000003EC,E2003412B802011234567890,-48",
		"+INV: 3034257BF468D480000003ED,,-60",
		"+INV: <ROUND FINISHED, ANT=1>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].TID != "E2003412B802011234567890" || tags[0].RSSI != -48 || tags[1].TID != "" {
		t.Fatalf("%+v", tags)
	}
	if _, err := parseInventory([]string{"+INV: <Antenna Error>"}); err == nil {
		t.Fatal("Antennenfehler sollte als Fehler kommen")
	}
}

func TestInvSettingsKeepsTail(t *testing.T) {
	got, err := invSettings([]string{"+INVS: 1,0,0,0,0,ALL,DUAL,-100"}, "12")
	if err != nil || got != "AT+INVS=0,1,12,0,0,ALL,DUAL,-100" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestInventoryDuplicatesMerged(t *testing.T) {
	tags, err := parseInventory([]string{
		"+INV: E280B11720007801144BEC88,E280B11720007801,-34",
		"+INV: E280B11720007801144BEC88,E280B11720007801,-30",
		"+INV: 3034257BF468D480000003ED,,-60",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].RSSI != -30 {
		t.Fatalf("%+v", tags)
	}
}

