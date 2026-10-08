// Package config lädt, prüft und speichert bridge.yaml.
//
// Die Datei ist die einzige Quelle der Gerätekonfiguration einer Station.
// ERPNext hält bewusst keine Stationsdaten (siehe docs/KONZEPT.md, Abschnitt 9).
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultListenPort ist die feste Konvention, die das Desk als Konstante kennt.
const DefaultListenPort = 8735

// Config ist der Inhalt von bridge.yaml.
type Config struct {
	Station        string   `yaml:"station" json:"station"`
	ListenPort     int      `yaml:"listen_port" json:"listen_port"`
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins"`
	Token          string   `yaml:"token,omitempty" json:"token,omitempty"`
	LogLevel       string   `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	// NoUpdateCheck schaltet die tägliche Frage nach einem neuen Release ab.
	// Der Knopf »Auf Updates prüfen« geht weiterhin.
	NoUpdateCheck bool           `yaml:"no_update_check,omitempty" json:"no_update_check,omitempty"`
	Devices       []DeviceConfig `yaml:"devices" json:"devices"`
	HTTPCompat    HTTPCompat     `yaml:"http_compat" json:"http_compat"`
}

// DeviceConfig beschreibt ein angeschlossenes Gerät.
type DeviceConfig struct {
	ID     string   `yaml:"id" json:"id"`
	Kind   string   `yaml:"kind" json:"kind"`     // scale, camera, scanner, printer, terminal
	Driver string   `yaml:"driver" json:"driver"` // pce_pb, …
	Port   PortSpec `yaml:"port" json:"port"`

	// Serielle Parameter (Waage, Scanner).
	Baud int `yaml:"baud,omitempty" json:"baud,omitempty"`

	// Waage.
	PollMS            int     `yaml:"poll_ms,omitempty" json:"poll_ms,omitempty"`
	StableSamples     int     `yaml:"stable_samples,omitempty" json:"stable_samples,omitempty"`
	StableToleranceKG float64 `yaml:"stable_tolerance_kg,omitempty" json:"stable_tolerance_kg,omitempty"`

	// Drucker: Address für raw_tcp (host:port, üblich 9100), Port.Path für
	// raw_file, Queue für system (Druckwarteschlange des Betriebssystems),
	// URI für ipp (ipp://, ipps://).
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
	Queue   string `yaml:"queue,omitempty" json:"queue,omitempty"`
	URI     string `yaml:"uri,omitempty" json:"uri,omitempty"`
	// InsecureTLS: Zertifikat des Druckers nicht prüfen (ipps mit
	// selbstsigniertem Zertifikat, wie bei Druckern üblich).
	InsecureTLS bool `yaml:"insecure_tls,omitempty" json:"insecure_tls,omitempty"`
	// Media und PrintScaling gehen als IPP-Auftragsattribute mit, wenn
	// gesetzt (z.B. iso_a6_105x148mm, fit), bei system/pdf als lp-Optionen.
	// Leer = Vorgabe des Druckers.
	Media        string `yaml:"media,omitempty" json:"media,omitempty"`
	PrintScaling string `yaml:"print_scaling,omitempty" json:"print_scaling,omitempty"`
	// Accept beschränkt einen ipp-Drucker auf ein Format ("pdf", "zpl" oder
	// "escpos"). Ein CUPS-Server meldet für jede Warteschlange beides, auch wenn
	// dahinter ein Bürodrucker steht, der mit Rohdaten nichts anfangen kann.
	// Bei Rohdruckern (raw_tcp, raw_file, system) sagt accept, welche Sprache
	// der Drucker spricht: "zpl" (Etiketten, Vorgabe) oder "escpos" (Bons).
	// Bei system unter Linux/macOS heißt "pdf": PDF durch den Treiber der
	// CUPS-Warteschlange drucken statt roh.
	Accept string `yaml:"accept,omitempty" json:"accept,omitempty"`
	// Default: Standarddrucker dieses Arbeitsplatzes für Aufträge ohne
	// Geräteangabe (Schnelldruck im Desk).
	Default bool `yaml:"default,omitempty" json:"default,omitempty"`

	// Terminal (ws_forward): Address ist das Ziel im LAN (host:port des
	// Zahlungsterminals), LocalPort der Port auf 127.0.0.1/::1, den die Kasse
	// als Terminal-Adresse benutzt.
	LocalPort int `yaml:"local_port,omitempty" json:"local_port,omitempty"`
}

// PortSpec wählt einen seriellen Port entweder über den Pfad oder über
// USB-Merkmale. In YAML darf er auch als einfacher String stehen.
type PortSpec struct {
	Path  string     `yaml:"path,omitempty" json:"path,omitempty"`
	Match *PortMatch `yaml:"match,omitempty" json:"match,omitempty"`
}

// PortMatch identifiziert ein USB-Seriell-Gerät plattformneutral.
type PortMatch struct {
	VID    string `yaml:"vid" json:"vid"`
	PID    string `yaml:"pid" json:"pid"`
	Serial string `yaml:"serial,omitempty" json:"serial,omitempty"`
}

// UnmarshalYAML erlaubt `port: /dev/waage` als Kurzform.
func (p *PortSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		p.Path = n.Value
		return nil
	}
	type plain PortSpec
	return n.Decode((*plain)(p))
}

// String liefert eine lesbare Kurzform für Logs und Oberfläche.
func (p PortSpec) String() string {
	if p.Path != "" {
		return p.Path
	}
	if p.Match != nil {
		s := "usb " + p.Match.VID + ":" + p.Match.PID
		if p.Match.Serial != "" {
			s += " #" + p.Match.Serial
		}
		return s
	}
	return "(kein Port)"
}

// HTTPCompat ist die Übergangsroute für den Frappe-Server (/weight usw.).
type HTTPCompat struct {
	Enabled   bool      `yaml:"enabled" json:"enabled"`
	Listen    string    `yaml:"listen,omitempty" json:"listen,omitempty"`
	BasicAuth BasicAuth `yaml:"basic_auth,omitempty" json:"basic_auth"`
	// ScaleDevice wählt die Waage für /weight; leer = erste Waage.
	ScaleDevice string `yaml:"scale_device,omitempty" json:"scale_device,omitempty"`
	// LegacyUpstream: alles außer /weight wird dorthin weitergereicht,
	// z.B. an die Flask-App für /shot und /health, bis die Kamera portiert ist.
	LegacyUpstream string `yaml:"legacy_upstream,omitempty" json:"legacy_upstream,omitempty"`
}

// BasicAuth für die HTTP-Kompat-Route. Leer = keine Prüfung
// (dann muss der Reverse Proxy davor authentifizieren).
type BasicAuth struct {
	User     string `yaml:"user,omitempty" json:"user,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

// Bekannte Treiber je Geräteklasse. Nicht enthaltene Treiber werden trotzdem
// akzeptiert, damit eine Config aus einem neueren Build nicht beim Laden
// scheitert; das Gerät meldet dann »Treiber nicht enthalten«.
var knownKinds = map[string]bool{"scale": true, "camera": true, "scanner": true, "printer": true, "terminal": true}

// Default liefert eine lauffähige Startkonfiguration ohne Geräte.
func Default() *Config {
	c := &Config{
		Station:    hostname(),
		ListenPort: DefaultListenPort,
		LogLevel:   "info",
		HTTPCompat: HTTPCompat{Listen: ":5000"},
	}
	return c
}

// ApplyDefaults füllt leere Felder mit sinnvollen Werten.
func (c *Config) ApplyDefaults() {
	if c.ListenPort == 0 {
		c.ListenPort = DefaultListenPort
	}
	if c.Station == "" {
		c.Station = hostname()
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.HTTPCompat.Listen == "" {
		c.HTTPCompat.Listen = ":5000"
	}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Kind == "scale" {
			if d.Baud == 0 {
				d.Baud = 9600
			}
			if d.PollMS == 0 {
				d.PollMS = 250
			}
			if d.StableSamples == 0 {
				d.StableSamples = 4
			}
			if d.StableToleranceKG == 0 {
				d.StableToleranceKG = 0.005
			}
		}
	}
	for i, o := range c.AllowedOrigins {
		c.AllowedOrigins[i] = NormalizeOrigin(o)
	}
}

// Validate prüft die Konfiguration und liefert alle Fehler auf einmal,
// damit die Oberfläche sie gemeinsam anzeigen kann.
func (c *Config) Validate() error {
	var errs []error
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		errs = append(errs, fmt.Errorf("listen_port %d ist kein gültiger Port", c.ListenPort))
	}
	for _, o := range c.AllowedOrigins {
		if err := validateOrigin(o); err != nil {
			errs = append(errs, err)
		}
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level %q unbekannt (debug, info, warn, error)", c.LogLevel))
	}
	seen := map[string]bool{}
	localPorts := map[int]string{}
	for i, d := range c.Devices {
		where := fmt.Sprintf("Gerät %d", i+1)
		if d.ID != "" {
			where = fmt.Sprintf("Gerät %q", d.ID)
		}
		switch {
		case d.ID == "":
			errs = append(errs, fmt.Errorf("%s: id fehlt", where))
		case !validID(d.ID):
			errs = append(errs, fmt.Errorf("%s: id darf nur a-z, 0-9, - und _ enthalten", where))
		case seen[d.ID]:
			errs = append(errs, fmt.Errorf("%s: id doppelt vergeben", where))
		}
		seen[d.ID] = true
		if !knownKinds[d.Kind] {
			errs = append(errs, fmt.Errorf("%s: kind %q unbekannt (scale, camera, scanner, printer, terminal)", where, d.Kind))
		}
		if d.Driver == "" {
			errs = append(errs, fmt.Errorf("%s: driver fehlt", where))
		}
		if d.Kind == "scale" || d.Kind == "scanner" {
			if d.Port.Path == "" && d.Port.Match == nil {
				errs = append(errs, fmt.Errorf("%s: port fehlt (path oder match)", where))
			}
			if d.Port.Path != "" && d.Port.Match != nil {
				errs = append(errs, fmt.Errorf("%s: port.path und port.match schließen sich aus", where))
			}
			if m := d.Port.Match; m != nil && (m.VID == "" || m.PID == "") {
				errs = append(errs, fmt.Errorf("%s: port.match braucht vid und pid", where))
			}
			if d.Baud < 50 || d.Baud > 4_000_000 {
				errs = append(errs, fmt.Errorf("%s: baud %d ungültig", where, d.Baud))
			}
		}
		if d.Kind == "printer" {
			if d.Driver != "ipp" && d.Accept != "" && d.Accept != "zpl" && d.Accept != "escpos" &&
				!(d.Driver == "system" && d.Accept == "pdf" && runtime.GOOS != "windows") {
				errs = append(errs, fmt.Errorf("%s: accept %q geht bei Rohdruckern nicht (zpl oder escpos)", where, d.Accept))
			}
			switch d.Driver {
			case "raw_tcp":
				if host, port, err := net.SplitHostPort(d.Address); err != nil || host == "" {
					errs = append(errs, fmt.Errorf("%s: address %q ist nicht host:port (z.B. 192.168.1.60:9100)", where, d.Address))
				} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
					errs = append(errs, fmt.Errorf("%s: address %q hat keinen gültigen Port", where, d.Address))
				}
			case "raw_file":
				if d.Port.Path == "" {
					errs = append(errs, fmt.Errorf("%s: port.path fehlt (z.B. /dev/usb/lp0)", where))
				}
			case "system":
				if strings.TrimSpace(d.Queue) == "" {
					errs = append(errs, fmt.Errorf("%s: queue fehlt (Name des Druckers im Betriebssystem)", where))
				}
				if d.Accept == "pdf" && runtime.GOOS == "windows" {
					errs = append(errs, fmt.Errorf("%s: PDF über einen Windows-Drucker geht nicht, nur roh (zpl oder escpos); für PDF den Drucker per ipp ansprechen", where))
				}
				errs = append(errs, checkPrintScaling(where, d.PrintScaling)...)
			case "ipp":
				u, err := url.Parse(d.URI)
				if err != nil || u.Host == "" || (u.Scheme != "ipp" && u.Scheme != "ipps") {
					errs = append(errs, fmt.Errorf("%s: uri %q ist keine ipp://- oder ipps://-Adresse", where, d.URI))
				}
				if d.Accept != "" && d.Accept != "pdf" && d.Accept != "zpl" && d.Accept != "escpos" {
					errs = append(errs, fmt.Errorf("%s: accept %q unbekannt (pdf, zpl, escpos oder leer)", where, d.Accept))
				}
				errs = append(errs, checkPrintScaling(where, d.PrintScaling)...)
			}
		}
		if d.Kind == "terminal" && d.Driver == "ws_forward" {
			if host, port, err := net.SplitHostPort(d.Address); err != nil || host == "" {
				errs = append(errs, fmt.Errorf("%s: address %q ist nicht host:port (z.B. 192.168.1.50:80)", where, d.Address))
			} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("%s: address %q hat keinen gültigen Port", where, d.Address))
			} else if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
				// Die volle Prüfung (eigenes Netz) läuft beim Verbinden, weil ein
				// Hostname erst dann aufgelöst wird. Loopback ist aber schon hier
				// sicher falsch: es wäre die Bridge selbst.
				errs = append(errs, fmt.Errorf("%s: address %q zeigt auf diesen Rechner, nicht auf das Terminal", where, d.Address))
			}
			switch {
			case d.LocalPort < 1 || d.LocalPort > 65535:
				errs = append(errs, fmt.Errorf("%s: local_port %d ist kein gültiger Port", where, d.LocalPort))
			case d.LocalPort == c.ListenPort:
				errs = append(errs, fmt.Errorf("%s: local_port darf nicht der Port der WS-API (%d) sein", where, d.LocalPort))
			case localPorts[d.LocalPort] != "":
				errs = append(errs, fmt.Errorf("%s: local_port %d ist schon %q zugeordnet", where, d.LocalPort, localPorts[d.LocalPort]))
			default:
				localPorts[d.LocalPort] = d.ID
			}
		}
		if d.Kind == "scale" {
			if d.PollMS < 50 || d.PollMS > 10_000 {
				errs = append(errs, fmt.Errorf("%s: poll_ms muss zwischen 50 und 10000 liegen", where))
			}
			if d.StableSamples < 1 || d.StableSamples > 50 {
				errs = append(errs, fmt.Errorf("%s: stable_samples muss zwischen 1 und 50 liegen", where))
			}
			if d.StableToleranceKG < 0 {
				errs = append(errs, fmt.Errorf("%s: stable_tolerance_kg darf nicht negativ sein", where))
			}
		}
	}
	h := c.HTTPCompat
	if h.Enabled {
		if _, port, err := net.SplitHostPort(h.Listen); err != nil {
			errs = append(errs, fmt.Errorf("http_compat.listen %q: %v", h.Listen, err))
		} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("http_compat.listen %q: ungültiger Port", h.Listen))
		} else if n == c.ListenPort {
			errs = append(errs, fmt.Errorf("http_compat.listen darf nicht den Port der WS-API (%d) verwenden", n))
		} else if id := localPorts[n]; id != "" {
			errs = append(errs, fmt.Errorf("http_compat.listen: Port %d ist schon der Terminal-Weiterleitung %q zugeordnet", n, id))
		}
		if (h.BasicAuth.User == "") != (h.BasicAuth.Password == "") {
			errs = append(errs, errors.New("http_compat.basic_auth: user und password nur gemeinsam"))
		}
		if h.LegacyUpstream != "" {
			u, err := url.Parse(h.LegacyUpstream)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("http_compat.legacy_upstream %q ist keine http(s)-URL", h.LegacyUpstream))
			}
		}
		if h.ScaleDevice != "" && !seen[h.ScaleDevice] {
			errs = append(errs, fmt.Errorf("http_compat.scale_device %q gibt es nicht", h.ScaleDevice))
		}
	}
	return errors.Join(errs...)
}

func validID(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return s != ""
}

// NormalizeOrigin bringt einen Origin in die Form, die Browser senden:
// Kleinbuchstaben, ohne Pfad und abschließenden Schrägstrich.
func NormalizeOrigin(o string) string {
	o = strings.TrimSpace(strings.ToLower(o))
	return strings.TrimRight(o, "/")
}

func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("allowed_origins: %q ist kein Origin (erwartet https://host[:port])", o)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("allowed_origins: %q darf keinen Pfad, Query oder Benutzer enthalten", o)
	}
	return nil
}

// Load liest die Datei. Fehlt sie, kommt die Default-Konfiguration zurück
// (exists=false), damit die Bridge startet und über die Oberfläche
// eingerichtet werden kann.
func Load(path string) (cfg *Config, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg = &Config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, true, fmt.Errorf("%s ungültig:\n%w", path, err)
	}
	return cfg, true, nil
}

// Save schreibt atomar: temporäre Datei im selben Verzeichnis, dann Rename.
// Die bisherige Fassung bleibt als .bak erhalten.
func Save(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# ERPNext Hardware Bridge – wird von der Oberfläche unter\n" +
		fmt.Sprintf("# http://localhost:%d/ geschrieben. Kommentare gehen beim Speichern verloren.\n", cfg.ListenPort)
	b = append([]byte(header), b...)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bridge-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Die Datei enthält Passwörter: nur für den Dienstbenutzer lesbar.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("Sicherung %s.bak: %w", path, err)
		}
	}
	return os.Rename(tmp.Name(), path)
}

// DefaultPath ist der plattformübliche Ort der Konfigurationsdatei.
func DefaultPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "ERPNextHardwareBridge", "bridge.yaml")
	}
	return "/etc/erpnext-hardware-bridge/bridge.yaml"
}

// Clone liefert eine tiefe Kopie.
func (c *Config) Clone() *Config {
	b, _ := yaml.Marshal(c)
	out := &Config{}
	_ = yaml.Unmarshal(b, out)
	return out
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "station"
	}
	return h
}

func checkPrintScaling(where, v string) []error {
	switch v {
	case "", "auto", "auto-fit", "fill", "fit", "none":
		return nil
	}
	return []error{fmt.Errorf("%s: print_scaling %q unbekannt (auto, auto-fit, fill, fit, none)", where, v)}
}
