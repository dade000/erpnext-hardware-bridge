package streamdeck

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Unter Linux spricht die Bridge das Stream Deck über /dev/hidrawN an,
// ohne libusb oder cgo. Gefunden wird es über sysfs (HID_ID, HID_UNIQ).
// Rechte: udev-Regel aus deploy/udev (Gruppe plugdev).

type hidraw struct{ f *os.File }

// OpenHID sucht das erste unterstützte Stream Deck, mit serial nur das mit
// dieser Seriennummer.
func OpenHID(serial string) (Conn, Info, error) {
	devs, err := listHID()
	if err != nil {
		return nil, Info{}, err
	}
	var unsupported string
	for _, d := range devs {
		if serial != "" && !strings.EqualFold(d.Serial, serial) {
			continue
		}
		if _, ok := ModelByPID(d.PID); !ok {
			unsupported = fmt.Sprintf("%04x", d.PID)
			continue
		}
		// O_NONBLOCK: die Datei läuft dann über den Poller der Laufzeit, so
		// beendet Close ein hängendes Read.
		f, err := os.OpenFile(d.Path, os.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			if os.IsPermission(err) {
				return nil, d, fmt.Errorf("%s: keine Berechtigung (udev-Regel für Stream Deck fehlt, siehe deploy/udev)", d.Path)
			}
			return nil, d, err
		}
		return &hidraw{f: f}, d, nil
	}
	if unsupported != "" {
		return nil, Info{}, fmt.Errorf("%w: Stream Deck mit Produkt-ID %s wird nicht unterstützt (nur Original V2 und MK.2)", ErrNotFound, unsupported)
	}
	return nil, Info{}, ErrNotFound
}

// listHID liefert alle hidraw-Geräte von Elgato.
func listHID() ([]Info, error) {
	entries, err := filepath.Glob("/sys/class/hidraw/hidraw*")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	var out []Info
	for _, e := range entries {
		vid, pid, uniq, ok := readUevent(filepath.Join(e, "device", "uevent"))
		if !ok || vid != VendorID {
			continue
		}
		out = append(out, Info{Path: "/dev/" + filepath.Base(e), PID: pid, Serial: uniq})
	}
	return out, nil
}

// readUevent liest HID_ID=0003:00000FD9:00000080 und HID_UNIQ=<seriennummer>.
func readUevent(path string) (vid, pid uint16, uniq string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "HID_ID":
			parts := strings.Split(v, ":")
			if len(parts) != 3 {
				return 0, 0, "", false
			}
			a, err1 := strconv.ParseUint(parts[1], 16, 32)
			b, err2 := strconv.ParseUint(parts[2], 16, 32)
			if err1 != nil || err2 != nil {
				return 0, 0, "", false
			}
			vid, pid, ok = uint16(a), uint16(b), true
		case "HID_UNIQ":
			uniq = v
		}
	}
	return vid, pid, uniq, ok
}

func (h *hidraw) Read(p []byte) (int, error) { return h.f.Read(p) }

func (h *hidraw) Write(p []byte) error {
	_, err := h.f.Write(p)
	return err
}

// hidioc baut HIDIOCSFEATURE/HIDIOCGFEATURE(len): _IOC(READ|WRITE, 'H', nr, len).
func hidioc(nr, n uintptr) uintptr { return 3<<30 | n<<16 | 'H'<<8 | nr }

func (h *hidraw) ioctl(nr uintptr, p []byte) (int, error) {
	rc, err := h.f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n uintptr
	var errno unix.Errno
	if cerr := rc.Control(func(fd uintptr) {
		n, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, hidioc(nr, uintptr(len(p))), uintptr(unsafe.Pointer(&p[0])))
	}); cerr != nil {
		return 0, cerr
	}
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

func (h *hidraw) SetFeature(p []byte) error {
	_, err := h.ioctl(0x06, p)
	return err
}

func (h *hidraw) GetFeature(p []byte) (int, error) { return h.ioctl(0x07, p) }

func (h *hidraw) Close() error { return h.f.Close() }
