package streamdeck

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"erpnext-hardware-bridge/internal/config"
)

// look ist der Zustand, in dem eine Taste gezeichnet wird.
type look int

const (
	lookNormal   look = iota
	lookIdle          // keine Seite hört zu: Taste ausgegraut
	lookPressed       // gedrückt: hell umrandet
	lookIdentify      // »Testen«: Tastennummer groß
)

var defaultBG = color.RGBA{0x33, 0x33, 0x33, 0xff}

func parseColor(s string) color.RGBA {
	if len(s) != 7 || s[0] != '#' {
		return defaultBG
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return defaultBG
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

// textColor: Schwarz auf hellem, Weiß auf dunklem Grund.
func textColor(bg color.RGBA) color.RGBA {
	lum := 0.299*float64(bg.R) + 0.587*float64(bg.G) + 0.114*float64(bg.B)
	if lum > 150 {
		return color.RGBA{0, 0, 0, 0xff}
	}
	return color.RGBA{0xff, 0xff, 0xff, 0xff}
}

func dim(c color.RGBA) color.RGBA {
	g := uint8((int(c.R) + int(c.G) + int(c.B)) / 3 / 3)
	return color.RGBA{g, g, g, 0xff}
}

var (
	fontOnce sync.Once
	fontTTF  *opentype.Font
	facesMu  sync.Mutex
	faces    = map[float64]font.Face{}
)

func loadFont() { fontTTF, _ = opentype.Parse(gobold.TTF) }

func face(size float64) font.Face {
	fontOnce.Do(loadFont)
	facesMu.Lock()
	defer facesMu.Unlock()
	if f := faces[size]; f != nil {
		return f
	}
	f, err := opentype.NewFace(fontTTF, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	faces[size] = f
	return f
}

// printable lässt Zeichen weg, die die Schrift nicht kennt (z.B. ✔ oder
// Emoji); sie kämen sonst als leeres Kästchen.
func printable(s string) string {
	fontOnce.Do(loadFont)
	var buf sfnt.Buffer
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == ' ' {
			return r
		}
		if i, err := fontTTF.GlyphIndex(&buf, r); err != nil || i == 0 {
			return -1
		}
		return r
	}, s)
}

// layout bricht die Beschriftung in Zeilen um (»\n« erzwingt einen Umbruch)
// und wählt die größte Schrift, mit der alles in die Taste passt.
func layout(label string, width, height int) (font.Face, []string) {
	for _, size := range []float64{18, 16, 14, 12, 11, 10} {
		f := face(size)
		lineH := f.Metrics().Height.Ceil()
		maxLines := max(1, height/lineH)
		lines, ok := wrap(f, label, width)
		if ok && len(lines) <= maxLines {
			return f, lines
		}
	}
	f := face(10)
	lines, _ := wrap(f, label, width)
	maxLines := max(1, height/f.Metrics().Height.Ceil())
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	for i, l := range lines {
		lines[i] = truncate(f, l, width)
	}
	return f, lines
}

func wrap(f font.Face, label string, width int) ([]string, bool) {
	var lines []string
	ok := true
	for _, para := range strings.Split(label, "\n") {
		words := strings.Fields(para)
		cur := ""
		for _, w := range words {
			if font.MeasureString(f, w).Ceil() > width {
				ok = false
			}
			try := w
			if cur != "" {
				try = cur + " " + w
			}
			if cur != "" && font.MeasureString(f, try).Ceil() > width {
				lines = append(lines, cur)
				cur = w
				continue
			}
			cur = try
		}
		lines = append(lines, cur)
	}
	return lines, ok
}

func truncate(f font.Face, s string, width int) string {
	r := []rune(s)
	for len(r) > 1 && font.MeasureString(f, string(r)).Ceil() > width {
		r = r[:len(r)-1]
	}
	return string(r)
}

// renderKey zeichnet eine Taste. k ist nil für eine unbelegte Taste.
func renderKey(k *config.DeckKey, idx, size int, l look) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0, 0, 0, 0xff}
	label := ""
	if k != nil {
		bg, label = parseColor(k.Color), k.Label
	}
	if l == lookIdentify {
		bg, label = color.RGBA{0x1f, 0x4e, 0x9e, 0xff}, strconv.Itoa(idx+1)
	}
	if l == lookIdle && k != nil {
		bg = dim(bg)
	}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	fg := textColor(bg)
	if l == lookIdle {
		fg = color.RGBA{0x99, 0x99, 0x99, 0xff}
	}
	if l == lookPressed {
		ring := color.RGBA{0xff, 0xff, 0xff, 0xff}
		if fg.R == 0xff {
			ring = color.RGBA{0xff, 0xd0, 0x00, 0xff}
		}
		w := max(3, size/18)
		for _, r := range []image.Rectangle{
			image.Rect(0, 0, size, w), image.Rect(0, size-w, size, size),
			image.Rect(0, 0, w, size), image.Rect(size-w, 0, size, size),
		} {
			draw.Draw(img, r, &image.Uniform{ring}, image.Point{}, draw.Src)
		}
	}
	if label == "" {
		return img
	}
	label = strings.TrimSpace(printable(label))
	if label == "" {
		return img
	}
	pad := size / 12
	f, lines := layout(label, size-2*pad, size-2*pad)
	if l == lookIdentify {
		f, lines = face(32), []string{label}
	}
	m := f.Metrics()
	lineH := m.Height.Ceil()
	top := (size-lineH*len(lines))/2 + m.Ascent.Ceil()
	d := &font.Drawer{Dst: img, Src: &image.Uniform{fg}, Face: f}
	for i, line := range lines {
		w := font.MeasureString(f, line).Ceil()
		d.Dot = fixed.P((size-w)/2, top+i*lineH)
		d.DrawString(line)
	}
	return img
}

// flip dreht das Bild um 180°: das Stream Deck zeigt es sonst kopfüber.
func flip(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	out := image.NewRGBA(b)
	w, h := b.Dx(), b.Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.SetRGBA(w-1-x, h-1-y, src.RGBAAt(x, y))
		}
	}
	return out
}

func encodeJPEG(img image.Image) []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92})
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
