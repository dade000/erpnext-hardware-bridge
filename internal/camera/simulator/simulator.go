// Package simulator ist eine Kamera ohne Kamera: Live-Bild und Fotos sind ein
// bewegtes Testbild. Gedacht für Tests und um die Photo Station ohne
// angeschlossene Kamera durchzuspielen (Treiber "simulator").
package simulator

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"

	"time"

	"erpnext-hardware-bridge/internal/camera"
	"erpnext-hardware-bridge/internal/config"
)

// Formate, die der Simulator wie eine Canon anbietet.
var formats = []string{"Large Fine JPEG", "Medium Fine JPEG", "Small Fine JPEG", "RAW"}

// Backend erzeugt Testbilder.
type Backend struct {
	open         bool
	format       string
	target       string
	viewOn       bool
	frame        int
	CaptureDelay time.Duration
}

// Open passt zu camera.Opener.
func Open(config.DeviceConfig) camera.Backend {
	return &Backend{format: formats[2], target: "Memory card", CaptureDelay: 300 * time.Millisecond}
}

func (b *Backend) Open() (camera.Info, error) {
	b.open = true
	return camera.Info{Model: "Simulator (Testbild)", Port: "sim:", Library: "simulator"}, nil
}

func (b *Backend) Close() { b.open = false; b.viewOn = false }

func (b *Backend) Capture() ([]byte, error) {
	if strings.EqualFold(b.format, "RAW") {
		return nil, camera.Errorf(false, "not_jpeg", "Kamera hat kein JPEG geliefert (IMG_0001.CR2)")
	}
	time.Sleep(b.CaptureDelay)
	w, h := 2400, 1600
	if strings.HasPrefix(b.format, "Large") {
		w, h = 4800, 3200
	} else if strings.HasPrefix(b.format, "Medium") {
		w, h = 3600, 2400
	}
	b.frame++
	return render(w, h, b.frame, 90)
}

func (b *Backend) Preview() ([]byte, error) {
	b.viewOn = true
	b.frame++
	return render(960, 640, b.frame, 70)
}

func (b *Backend) Config(name string) (string, []string, error) {
	switch name {
	case "imageformat":
		return b.format, formats, nil
	case "batterylevel":
		return "100%", nil, nil
	case "capturetarget":
		return b.target, []string{"Internal RAM", "Memory card"}, nil
	case "viewfinder":
		if b.viewOn {
			return "1", nil, nil
		}
		return "0", nil, nil
	}
	return "", nil, camera.Errorf(false, "camera_error", "Einstellung %s: Not supported (-6)", name)
}

func (b *Backend) SetConfig(name, value string) error {
	switch name {
	case "imageformat":
		for _, f := range formats {
			if strings.EqualFold(f, value) {
				b.format = f
				return nil
			}
		}
		return camera.Errorf(false, "bad_request", "imageformat: »%s« kennt die Kamera nicht", value)
	case "capturetarget":
		b.target = value
		return nil
	case "viewfinder":
		b.viewOn = value == "1"
		return nil
	case "autofocusdrive":
		return nil
	}
	return camera.Errorf(false, "camera_error", "Einstellung %s: Not supported (-6)", name)
}

// render malt ein Testbild: Farbverlauf, ein wandernder Balken und ein
// Raster, damit Ruckeln und Verzerrung auffallen.
func render(w, h, n, quality int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bar := (n * w / 60) % w
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(80 + 120*x/w), uint8(90 + 100*y/h), 150, 255}
			if (x/(w/12)+y/(h/8))%2 == 0 {
				c.R, c.G, c.B = c.R-30, c.G-30, c.B-30
			}
			if x >= bar && x < bar+w/40 {
				c = color.RGBA{240, 200, 40, 255}
			}
			img.Pix[y*img.Stride+x*4+0] = c.R
			img.Pix[y*img.Stride+x*4+1] = c.G
			img.Pix[y*img.Stride+x*4+2] = c.B
			img.Pix[y*img.Stride+x*4+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
