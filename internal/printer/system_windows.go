//go:build windows

package printer

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// systemSink druckt roh über den Windows-Spooler (Datentyp RAW), also am
// Druckertreiber vorbei direkt in die Sprache des Druckers.
type systemSink struct{ queue string }

func newSystemSink(queue string) Sink { return &systemSink{queue: queue} }

func (s *systemSink) Target() string { return "Drucker " + s.queue }

var (
	winspool             = syscall.NewLazyDLL("winspool.drv")
	procOpenPrinter      = winspool.NewProc("OpenPrinterW")
	procClosePrinter     = winspool.NewProc("ClosePrinter")
	procStartDocPrinter  = winspool.NewProc("StartDocPrinterW")
	procEndDocPrinter    = winspool.NewProc("EndDocPrinter")
	procStartPagePrinter = winspool.NewProc("StartPagePrinter")
	procEndPagePrinter   = winspool.NewProc("EndPagePrinter")
	procWritePrinter     = winspool.NewProc("WritePrinter")
	procEnumPrinters     = winspool.NewProc("EnumPrintersW")
)

type docInfo1 struct {
	docName    *uint16
	outputFile *uint16
	datatype   *uint16
}

func (s *systemSink) open() (syscall.Handle, error) {
	if s.queue == "" {
		return 0, errors.New("kein Drucker konfiguriert")
	}
	name, err := syscall.UTF16PtrFromString(s.queue)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	r, _, e := procOpenPrinter.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&h)), 0)
	if r == 0 {
		return 0, fmt.Errorf("Drucker %q nicht gefunden (%v)", s.queue, e)
	}
	return h, nil
}

func (s *systemSink) Check(context.Context) error {
	h, err := s.open()
	if err != nil {
		return err
	}
	procClosePrinter.Call(uintptr(h))
	return nil
}

func (s *systemSink) Write(_ context.Context, title string, data []byte) error {
	h, err := s.open()
	if err != nil {
		return err
	}
	defer procClosePrinter.Call(uintptr(h))

	docName, _ := syscall.UTF16PtrFromString(title)
	raw, _ := syscall.UTF16PtrFromString("RAW")
	di := docInfo1{docName: docName, datatype: raw}
	if r, _, e := procStartDocPrinter.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&di))); r == 0 {
		return fmt.Errorf("StartDocPrinter: %v", e)
	}
	defer procEndDocPrinter.Call(uintptr(h))
	if r, _, e := procStartPagePrinter.Call(uintptr(h)); r == 0 {
		return fmt.Errorf("StartPagePrinter: %v", e)
	}
	defer procEndPagePrinter.Call(uintptr(h))

	for len(data) > 0 {
		var written uint32
		r, _, e := procWritePrinter.Call(uintptr(h), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&written)))
		if r == 0 {
			return fmt.Errorf("WritePrinter: %v", e)
		}
		if written == 0 {
			return errors.New("WritePrinter hat nichts geschrieben")
		}
		data = data[written:]
	}
	return nil
}

// printerInfo4 entspricht PRINTER_INFO_4W.
type printerInfo4 struct {
	printerName *uint16
	serverName  *uint16
	attributes  uint32
}

const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
)

// ListQueues nennt die Drucker des Betriebssystems (für die Auswahl in der
// Oberfläche).
func ListQueues(context.Context) ([]string, error) {
	flags := uintptr(printerEnumLocal | printerEnumConnections)
	var needed, count uint32
	procEnumPrinters.Call(flags, 0, 4, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if needed == 0 {
		return nil, nil
	}
	buf := make([]byte, needed)
	r, _, e := procEnumPrinters.Call(flags, 0, 4, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if r == 0 {
		return nil, fmt.Errorf("EnumPrinters: %v", e)
	}
	infos := unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buf[0])), count)
	queues := make([]string, 0, count)
	for _, info := range infos {
		if info.printerName != nil {
			queues = append(queues, utf16PtrToString(info.printerName))
		}
	}
	return queues, nil
}

func utf16PtrToString(p *uint16) string {
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; n++ {
		ptr = unsafe.Add(ptr, 2)
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
