//go:build linux

// Package gphoto steuert eine Kamera über libgphoto2 an (Canon EOS u.a.).
//
// Die Bibliothek wird zur Laufzeit geladen (purego, ohne cgo): das Binary
// bleibt ohne C-Compiler baubar, und eine Station ohne libgphoto2 startet
// trotzdem – das Kameragerät meldet dann, was fehlt. Auf dem Raspberry Pi
// der Photo Station ist libgphoto2 schon da (lief bisher unter der Flask-App).
//
// Verhalten wie in app.py: Sitzung offen halten, Fotos ins RAM der Kamera,
// JPEG bevorzugen (bei RAW+JPEG das JPEG nehmen), Live-Bild über
// capture_preview.
package gphoto

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"erpnext-hardware-bridge/internal/camera"
	"erpnext-hardware-bridge/internal/config"
)

// Aus gphoto2-result.h / gphoto2-port-result.h.
const (
	gpOK                 = 0
	gpErrorIO            = -7
	gpErrorTimeout       = -10
	gpErrorIOUSBFind     = -52
	gpErrorIOUSBClaim    = -53
	gpErrorModelNotFound = -105
	gpErrorCameraBusy    = -110
	gpErrorOSFailure     = -114
)

// Aus gphoto2-camera.h / gphoto2-widget.h / gphoto2-file.h.
const (
	captureImage = 0

	fileTypeNormal = 1

	eventUnknown     = 0
	eventTimeout     = 1
	eventFileAdded   = 2
	eventFolderAdded = 3
	eventCaptureDone = 4
	eventFileChanged = 5

	widgetText   = 2
	widgetRange  = 3
	widgetToggle = 4
	widgetRadio  = 5
	widgetMenu   = 6
	widgetDate   = 8

	// CameraFilePath: char name[128]; char folder[1024];
	pathNameLen   = 128
	pathFolderLen = 1024
	// CameraAbilities beginnt mit char model[128]; der Rest ist für uns
	// uninteressant, braucht aber Platz (gut 4 KB, großzügig gerechnet).
	abilitiesSize = 16 << 10
)

type lib struct {
	contextNew         func() unsafe.Pointer
	cameraNew          func(cam *unsafe.Pointer) int32
	cameraInit         func(cam, ctx unsafe.Pointer) int32
	cameraExit         func(cam, ctx unsafe.Pointer) int32
	cameraUnref        func(cam unsafe.Pointer) int32
	cameraCapture      func(cam unsafe.Pointer, typ int32, path unsafe.Pointer, ctx unsafe.Pointer) int32
	cameraPreview      func(cam, file, ctx unsafe.Pointer) int32
	cameraWaitEvent    func(cam unsafe.Pointer, timeoutMS int32, typ *int32, data *unsafe.Pointer, ctx unsafe.Pointer) int32
	cameraFileGet      func(cam unsafe.Pointer, folder, name string, typ int32, file, ctx unsafe.Pointer) int32
	cameraFileDelete   func(cam unsafe.Pointer, folder, name string, ctx unsafe.Pointer) int32
	cameraGetSingle    func(cam unsafe.Pointer, name string, widget *unsafe.Pointer, ctx unsafe.Pointer) int32
	cameraSetSingle    func(cam unsafe.Pointer, name string, widget, ctx unsafe.Pointer) int32
	cameraGetAbilities func(cam unsafe.Pointer, abilities unsafe.Pointer) int32
	cameraGetPortInfo  func(cam unsafe.Pointer, info *unsafe.Pointer) int32
	portInfoGetPath    func(info unsafe.Pointer, path *unsafe.Pointer) int32
	widgetGetType      func(w unsafe.Pointer, typ *int32) int32
	widgetSetValue     func(w unsafe.Pointer, value unsafe.Pointer) int32
	widgetGetValue     func(w unsafe.Pointer, value unsafe.Pointer) int32
	widgetCountChoices func(w unsafe.Pointer) int32
	widgetGetChoice    func(w unsafe.Pointer, i int32, choice *unsafe.Pointer) int32
	widgetFree         func(w unsafe.Pointer) int32
	fileNew            func(file *unsafe.Pointer) int32
	fileUnref          func(file unsafe.Pointer) int32
	fileGetDataAndSize func(file unsafe.Pointer, data *unsafe.Pointer, size *uintptr) int32
	resultAsString     func(code int32) string
	libraryVersion     func(verbose int32) unsafe.Pointer
	free               func(p unsafe.Pointer)

	version string
}

var (
	loadOnce sync.Once
	loaded   *lib
	loadErr  error
)

// Bibliotheksnamen in der Reihenfolge des Versuchs (Debian/Ubuntu/Raspberry
// Pi OS liefern libgphoto2.so.6).
var libNames = []string{"libgphoto2.so.6", "libgphoto2.so"}

func load() (*lib, error) {
	loadOnce.Do(func() {
		var h uintptr
		var err error
		for _, n := range libNames {
			if h, err = purego.Dlopen(n, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
				break
			}
		}
		if err != nil {
			loadErr = fmt.Errorf("libgphoto2 nicht gefunden (Paket libgphoto2-6 installieren): %v", err)
			return
		}
		libc, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("libc nicht ladbar: %v", err)
			return
		}
		l := &lib{}
		defer func() {
			// RegisterLibFunc bricht mit panic ab, wenn ein Symbol fehlt
			// (zu alte libgphoto2).
			if r := recover(); r != nil {
				loadErr = fmt.Errorf("libgphoto2 zu alt oder unvollständig: %v", r)
			}
		}()
		reg := func(fn any, name string) { purego.RegisterLibFunc(fn, h, name) }
		reg(&l.contextNew, "gp_context_new")
		reg(&l.cameraNew, "gp_camera_new")
		reg(&l.cameraInit, "gp_camera_init")
		reg(&l.cameraExit, "gp_camera_exit")
		reg(&l.cameraUnref, "gp_camera_unref")
		reg(&l.cameraCapture, "gp_camera_capture")
		reg(&l.cameraPreview, "gp_camera_capture_preview")
		reg(&l.cameraWaitEvent, "gp_camera_wait_for_event")
		reg(&l.cameraFileGet, "gp_camera_file_get")
		reg(&l.cameraFileDelete, "gp_camera_file_delete")
		reg(&l.cameraGetSingle, "gp_camera_get_single_config")
		reg(&l.cameraSetSingle, "gp_camera_set_single_config")
		reg(&l.cameraGetAbilities, "gp_camera_get_abilities")
		reg(&l.cameraGetPortInfo, "gp_camera_get_port_info")
		reg(&l.portInfoGetPath, "gp_port_info_get_path")
		reg(&l.widgetGetType, "gp_widget_get_type")
		reg(&l.widgetSetValue, "gp_widget_set_value")
		reg(&l.widgetGetValue, "gp_widget_get_value")
		reg(&l.widgetCountChoices, "gp_widget_count_choices")
		reg(&l.widgetGetChoice, "gp_widget_get_choice")
		reg(&l.widgetFree, "gp_widget_free")
		reg(&l.fileNew, "gp_file_new")
		reg(&l.fileUnref, "gp_file_unref")
		reg(&l.fileGetDataAndSize, "gp_file_get_data_and_size")
		reg(&l.resultAsString, "gp_result_as_string")
		reg(&l.libraryVersion, "gp_library_version")
		purego.RegisterLibFunc(&l.free, libc, "free")
		if v := l.libraryVersion(0); v != nil {
			l.version = cString(*(*unsafe.Pointer)(v))
		}
		loaded = l
	})
	return loaded, loadErr
}

// Version liefert die Version der installierten libgphoto2 ("" ohne Bibliothek).
func Version() string {
	l, err := load()
	if err != nil {
		return ""
	}
	return l.version
}

func cString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

func fixedString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// Backend ist eine Sitzung zu einer Kamera.
type Backend struct {
	l   *lib
	ctx unsafe.Pointer
	cam unsafe.Pointer
}

// Open passt zu camera.Opener.
func Open(config.DeviceConfig) camera.Backend { return &Backend{} }

func (b *Backend) err(rc int32, what string) error {
	msg := fmt.Sprintf("%s: %s (%d)", what, b.l.resultAsString(rc), rc)
	switch rc {
	case gpErrorModelNotFound:
		return camera.Errorf(true, "camera_offline", "Keine Kamera gefunden – eingeschaltet und per USB verbunden?")
	case gpErrorIOUSBClaim:
		return camera.Errorf(true, "camera_offline",
			"Kamera ist von einem anderen Programm belegt (läuft noch die Flask-App, gphoto2 oder gvfs?)")
	case gpErrorIOUSBFind, gpErrorOSFailure:
		return camera.Errorf(true, "camera_offline", "%s", msg)
	case gpErrorCameraBusy:
		return camera.Errorf(false, "busy", "Kamera ist beschäftigt, gleich nochmal versuchen")
	}
	// Ein- und Ausgabefehler (-7 und der Bereich -31 … -60 von libgphoto2_port)
	// heißen fast immer: Kamera aus, abgesteckt oder eingeschlafen.
	if rc == gpErrorIO || (rc <= -31 && rc >= -60) {
		return camera.Errorf(true, "camera_offline", "%s", msg)
	}
	return camera.Errorf(false, "camera_error", "%s", msg)
}

func (b *Backend) Open() (camera.Info, error) {
	l, err := load()
	if err != nil {
		return camera.Info{}, camera.Errorf(true, "unsupported", "%v", err)
	}
	b.l = l
	if b.ctx == nil {
		b.ctx = l.contextNew()
	}
	var cam unsafe.Pointer
	if rc := l.cameraNew(&cam); rc < gpOK {
		return camera.Info{}, b.err(rc, "gp_camera_new")
	}
	if rc := l.cameraInit(cam, b.ctx); rc < gpOK {
		l.cameraUnref(cam)
		return camera.Info{}, b.err(rc, "Kamera öffnen")
	}
	b.cam = cam

	info := camera.Info{Library: "libgphoto2 " + l.version}
	ab := make([]byte, abilitiesSize)
	if l.cameraGetAbilities(cam, unsafe.Pointer(&ab[0])) >= gpOK {
		info.Model = fixedString(ab[:pathNameLen])
	}
	var pi unsafe.Pointer
	if l.cameraGetPortInfo(cam, &pi) >= gpOK && pi != nil {
		var p unsafe.Pointer
		if l.portInfoGetPath(pi, &p) >= gpOK {
			info.Port = cString(p)
		}
	}
	if info.Model == "" {
		info.Model = "Kamera"
	}
	return info, nil
}

func (b *Backend) Close() {
	if b.cam == nil {
		return
	}
	b.l.cameraExit(b.cam, b.ctx)
	b.l.cameraUnref(b.cam)
	b.cam = nil
}

type filePath struct{ folder, name string }

func isJPEG(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg":
		return true
	}
	return false
}

// drain holt liegengebliebene Events ab (z.B. von Einstellungsänderungen),
// damit sie die nächste Aufnahme nicht stören.
func (b *Backend) drain(maxWait time.Duration, collect func(filePath)) {
	deadline := time.Now().Add(maxWait)
	for i := 0; i < 100 && time.Now().Before(deadline); i++ {
		var typ int32
		var data unsafe.Pointer
		if rc := b.l.cameraWaitEvent(b.cam, 10, &typ, &data, b.ctx); rc < gpOK {
			return
		}
		if typ == eventFileAdded && data != nil && collect != nil {
			buf := unsafe.Slice((*byte)(data), pathNameLen+pathFolderLen)
			collect(filePath{name: fixedString(buf[:pathNameLen]), folder: fixedString(buf[pathNameLen:])})
		}
		if data != nil {
			b.l.free(data)
		}
		if typ == eventTimeout {
			return
		}
	}
}

func (b *Backend) Capture() ([]byte, error) {
	b.drain(200*time.Millisecond, nil)

	buf := make([]byte, pathNameLen+pathFolderLen)
	if rc := b.l.cameraCapture(b.cam, captureImage, unsafe.Pointer(&buf[0]), b.ctx); rc < gpOK {
		return nil, b.err(rc, "Auslösen")
	}
	files := []filePath{{name: fixedString(buf[:pathNameLen]), folder: fixedString(buf[pathNameLen:])}}

	// RAW+JPEG: das zweite Bild kommt als Event hinterher.
	if !isJPEG(files[0].name) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && !hasJPEG(files) {
			var typ int32
			var data unsafe.Pointer
			if rc := b.l.cameraWaitEvent(b.cam, 500, &typ, &data, b.ctx); rc < gpOK {
				break
			}
			if typ == eventFileAdded && data != nil {
				eb := unsafe.Slice((*byte)(data), pathNameLen+pathFolderLen)
				files = append(files, filePath{name: fixedString(eb[:pathNameLen]), folder: fixedString(eb[pathNameLen:])})
			}
			if data != nil {
				b.l.free(data)
			}
			if typ == eventCaptureDone {
				break
			}
		}
	}

	var img []byte
	var getErr error
	for _, f := range files {
		if img == nil && isJPEG(f.name) {
			img, getErr = b.download(f)
		}
	}
	for _, f := range files {
		// Im RAM der Kamera: löschen gibt den Platz frei, Fehler egal.
		b.l.cameraFileDelete(b.cam, f.folder, f.name, b.ctx)
	}
	if getErr != nil {
		return nil, getErr
	}
	if img == nil {
		return nil, camera.Errorf(false, "not_jpeg",
			"Kamera hat kein JPEG geliefert (%s) – Bildqualität an der Kamera bzw. in der Bridge auf JPEG stellen", files[0].name)
	}
	return img, nil
}

func hasJPEG(fs []filePath) bool {
	for _, f := range fs {
		if isJPEG(f.name) {
			return true
		}
	}
	return false
}

func (b *Backend) download(f filePath) ([]byte, error) {
	var file unsafe.Pointer
	if rc := b.l.fileNew(&file); rc < gpOK {
		return nil, b.err(rc, "gp_file_new")
	}
	defer b.l.fileUnref(file)
	if rc := b.l.cameraFileGet(b.cam, f.folder, f.name, fileTypeNormal, file, b.ctx); rc < gpOK {
		return nil, b.err(rc, "Foto laden")
	}
	return fileBytes(b.l, file)
}

func fileBytes(l *lib, file unsafe.Pointer) ([]byte, error) {
	var data unsafe.Pointer
	var size uintptr
	if rc := l.fileGetDataAndSize(file, &data, &size); rc < gpOK {
		return nil, fmt.Errorf("gp_file_get_data_and_size: %d", rc)
	}
	if data == nil || size == 0 {
		return nil, camera.Errorf(false, "camera_error", "Kamera hat ein leeres Bild geliefert")
	}
	// Kopie: der Speicher gehört der CameraFile und ist nach unref weg.
	return append([]byte(nil), unsafe.Slice((*byte)(data), size)...), nil
}

func (b *Backend) Preview() ([]byte, error) {
	var file unsafe.Pointer
	if rc := b.l.fileNew(&file); rc < gpOK {
		return nil, b.err(rc, "gp_file_new")
	}
	defer b.l.fileUnref(file)
	if rc := b.l.cameraPreview(b.cam, file, b.ctx); rc < gpOK {
		return nil, b.err(rc, "Live-Bild")
	}
	return fileBytes(b.l, file)
}

func (b *Backend) widget(name string) (unsafe.Pointer, int32, error) {
	var w unsafe.Pointer
	if rc := b.l.cameraGetSingle(b.cam, name, &w, b.ctx); rc < gpOK {
		return nil, 0, b.err(rc, "Einstellung "+name)
	}
	var typ int32
	b.l.widgetGetType(w, &typ)
	return w, typ, nil
}

func (b *Backend) Config(name string) (string, []string, error) {
	w, typ, err := b.widget(name)
	if err != nil {
		return "", nil, err
	}
	defer b.l.widgetFree(w)
	var value string
	switch typ {
	case widgetText, widgetRadio, widgetMenu:
		var p unsafe.Pointer
		if b.l.widgetGetValue(w, unsafe.Pointer(&p)) >= gpOK {
			value = cString(p)
		}
	case widgetToggle, widgetDate:
		var n int32
		if b.l.widgetGetValue(w, unsafe.Pointer(&n)) >= gpOK {
			value = strconv.Itoa(int(n))
		}
	}
	var choices []string
	if typ == widgetRadio || typ == widgetMenu {
		n := b.l.widgetCountChoices(w)
		for i := int32(0); i < n; i++ {
			var p unsafe.Pointer
			if b.l.widgetGetChoice(w, i, &p) >= gpOK {
				choices = append(choices, cString(p))
			}
		}
	}
	return value, choices, nil
}

func (b *Backend) SetConfig(name, value string) error {
	w, typ, err := b.widget(name)
	if err != nil {
		return err
	}
	defer b.l.widgetFree(w)
	switch typ {
	case widgetRadio, widgetMenu:
		// Den Text so übernehmen, wie die Kamera ihn kennt.
		n := b.l.widgetCountChoices(w)
		found := ""
		for i := int32(0); i < n; i++ {
			var p unsafe.Pointer
			if b.l.widgetGetChoice(w, i, &p) >= gpOK {
				if c := cString(p); strings.EqualFold(c, value) {
					found = c
					break
				}
			}
		}
		if found == "" {
			return camera.Errorf(false, "bad_request", "%s: »%s« kennt die Kamera nicht", name, value)
		}
		value = found
		fallthrough
	case widgetText:
		cs := append([]byte(value), 0)
		if rc := b.l.widgetSetValue(w, unsafe.Pointer(&cs[0])); rc < gpOK {
			return b.err(rc, "Einstellung "+name)
		}
	case widgetToggle, widgetDate:
		n, err := strconv.Atoi(value)
		if err != nil {
			return camera.Errorf(false, "bad_request", "%s: Zahl erwartet", name)
		}
		v := int32(n)
		if rc := b.l.widgetSetValue(w, unsafe.Pointer(&v)); rc < gpOK {
			return b.err(rc, "Einstellung "+name)
		}
	default:
		return errors.New(name + ": Einstellungstyp wird nicht unterstützt")
	}
	if rc := b.l.cameraSetSingle(b.cam, name, w, b.ctx); rc < gpOK {
		return b.err(rc, "Einstellung "+name)
	}
	return nil
}
