package metratec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/serialport"
)

// Das metratec-AT-Protokoll (UHF AT Protocol Guide, Vorlage für diesen Code:
// github.com/metratec/rfid-sdk-python, uhf_reader_at.py):
//
//	Befehl   AT+READ=USR,0,8<CR>
//	Antwort  [Echo der Befehlszeile, wenn ATE1]
//	         +READ: 3034257BF468D480000003EE,OK,0011223344556677
//	         OK            – oder ERROR, Ursache dann als <…> in einer Zeile davor
//
// Ereigniszeilen (+CINV, +HBT, +IEV) können jederzeit dazwischen kommen und
// werden übergangen; die Bridge startet selbst kein Dauer-Inventory.

// atConn liest zeilenweise vom Port; Reste zwischen zwei Read-Aufrufen
// bleiben im Puffer.
type atConn struct {
	port serialport.Port
	buf  []byte
	// log bekommt bei Level debug jeden Befehl und jede Antwortzeile; nil
	// oder quiet = nichts.
	log   *slog.Logger
	quiet bool
}

// atError ist eine ERROR-Antwort des Readers.
type atError struct {
	cmd   string
	msg   string
	lines []string // Antwortzeilen vor ERROR
}

func (e *atError) Error() string { return e.cmd + ": " + e.msg }

func isEvent(line string) bool {
	for _, p := range []string{"+CINV", "+CMINV", "+HBT", "+IEV"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// command schickt eine Befehlszeile und sammelt die Antwortzeilen bis OK.
// Fehler vom Typ *atError: der Reader hat ERROR gemeldet. *device.Error mit
// Code "timeout": keine vollständige Antwort. Alle anderen: Port kaputt.
func (c *atConn) command(cmd string, timeout time.Duration) ([]string, error) {
	_ = c.port.ResetInputBuffer()
	c.buf = c.buf[:0]
	trace := c.log != nil && !c.quiet && c.log.Enabled(context.Background(), slog.LevelDebug)
	if trace {
		c.log.Debug("AT →", "cmd", cmd)
	}
	if _, err := c.port.Write([]byte(cmd + "\r")); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var lines []string
	for {
		line, err := c.readLine(deadline)
		if err != nil {
			return nil, err
		}
		if trace && line != "" {
			c.log.Debug("AT ←", "line", line)
		}
		switch {
		case line == "":
			if trace {
				c.log.Debug("AT ← (Zeitüberschreitung)", "cmd", cmd)
			}
			return nil, device.Errf("timeout", "RFID-Reader hat auf "+cmd+" nicht rechtzeitig geantwortet")
		case line == cmd, isEvent(line):
			continue
		case line == "OK":
			return lines, nil
		case line == "ERROR":
			return nil, &atError{cmd: cmd, msg: errorText(lines, cmd), lines: lines}
		}
		lines = append(lines, line)
	}
}

// errorText holt die Ursache aus der letzten Zeile mit <…>.
func errorText(lines []string, cmd string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if a, b := strings.LastIndex(l, "<"), strings.LastIndex(l, ">"); a >= 0 && b > a {
			return l[a+1 : b]
		}
	}
	return cmd + " ERROR"
}

// readLine liefert die nächste nicht leere Zeile; "" ohne Fehler heißt
// Zeitüberschreitung.
func (c *atConn) readLine(deadline time.Time) (string, error) {
	chunk := make([]byte, 256)
	for {
		if i := bytes.IndexAny(c.buf, "\r\n"); i >= 0 {
			line := string(c.buf[:i])
			c.buf = c.buf[i+1:]
			if line == "" {
				continue
			}
			return line, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return "", nil
		}
		if err := c.port.SetReadTimeout(min(left, 200*time.Millisecond)); err != nil {
			return "", err
		}
		n, err := c.port.Read(chunk)
		if err != nil {
			return "", err
		}
		c.buf = append(c.buf, chunk[:n]...)
		if len(c.buf) > 64<<10 {
			c.buf = c.buf[:0] // Müll ohne Zeilenende
		}
	}
}

// Tag ist ein Transponder aus dem Inventory.
type Tag struct {
	EPC  string `json:"epc"`
	TID  string `json:"tid"`
	RSSI int    `json:"rssi"`
}

// parseInventory wertet +INV-Zeilen aus (Einstellung EPC,TID,RSSI).
func parseInventory(lines []string) ([]Tag, error) {
	tags := []Tag{}
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, "+INV: ")
		if !ok {
			continue
		}
		if strings.HasPrefix(rest, "<") {
			up := strings.ToUpper(rest)
			if strings.Contains(up, "NO TAGS") || strings.Contains(up, "ROUND FINISHED") {
				continue
			}
			return nil, device.Errf("reader_error", strings.Trim(rest, "<>"))
		}
		f := strings.Split(rest, ",")
		t := Tag{EPC: strings.ToUpper(f[0])}
		if len(f) > 1 && isHex(f[1]) {
			t.TID = strings.ToUpper(f[1])
		}
		if len(f) > 2 {
			t.RSSI, _ = strconv.Atoi(strings.TrimSpace(f[2]))
		}
		tags = append(tags, t)
	}
	return dedupe(tags), nil
}

// dedupe führt Mehrfachmeldungen desselben Tags zusammen (gleicher EPC):
// der DeskID meldet einen Tag in einer Runde gelegentlich zweimal, die
// Fotostation zeigte dann "2 Tags am Reader". Behalten wird die Meldung mit
// TID, bei Gleichstand die stärkere.
func dedupe(tags []Tag) []Tag {
	out := tags[:0]
	idx := map[string]int{}
	for _, t := range tags {
		i, seen := idx[t.EPC]
		switch {
		case !seen:
			idx[t.EPC] = len(out)
			out = append(out, t)
		case out[i].TID == "" && t.TID != "",
			len(t.TID) == len(out[i].TID) && t.RSSI > out[i].RSSI:
			out[i] = t
		}
	}
	return out
}

// tagResult ist eine Zeile wie "+WRT: <EPC>,OK" oder "+READ: <EPC>,OK,<hex>".
type tagResult struct {
	EPC    string
	Status string
	Data   string
}

func parseTagResults(lines []string, prefix string) []tagResult {
	var out []tagResult
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, prefix)
		if !ok || strings.HasPrefix(rest, "<") {
			continue
		}
		f := strings.SplitN(rest, ",", 3)
		r := tagResult{EPC: f[0]}
		if len(f) > 1 {
			r.Status = f[1]
		}
		if len(f) > 2 {
			r.Data = f[2]
		}
		out = append(out, r)
	}
	return out
}

func isHex(s string) bool {
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// readerInfo aus ATI: "+SW: DeskID_UHF_v2_E 0105", "+HW: …", "+SERIAL: …".
type readerInfo struct {
	Firmware string `json:"firmware"`
	Hardware string `json:"hardware"`
	Serial   string `json:"serial"`
}

func parseInfo(lines []string) readerInfo {
	var ri readerInfo
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+SW: "):
			ri.Firmware = strings.TrimPrefix(l, "+SW: ")
		case strings.HasPrefix(l, "+HW: "):
			ri.Hardware = strings.TrimPrefix(l, "+HW: ")
		case strings.HasPrefix(l, "+SERIAL: "):
			ri.Serial = strings.TrimPrefix(l, "+SERIAL: ")
		}
	}
	return ri
}

// invSettings setzt in "+INVS: 0,1,0,0,…" die Felder ONT=0, RSSI=1 und TID
// (tid: "1" = Vorgabelänge der Firmware oder eine Byte-Anzahl wie "12") und
// lässt den Rest (je nach Firmware unterschiedlich viele) stehen.
func invSettings(lines []string, tid string) (string, error) {
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, "+INVS: ")
		if !ok {
			continue
		}
		f := strings.Split(rest, ",")
		if len(f) < 3 {
			break
		}
		f[0], f[1], f[2] = "0", "1", tid
		return "AT+INVS=" + strings.Join(f, ","), nil
	}
	return "", fmt.Errorf("unerwartete Antwort auf AT+INVS?: %q", lines)
}
