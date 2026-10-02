// Package update prüft auf neue Releases der Bridge, lädt sie herunter und
// spielt sie ein.
//
// Quelle sind die GitHub-Releases des Repos: Jeder Versions-Tag baut dort die
// Binaries aller Plattformen, eine Prüfsummen-Datei und deren Signatur. Die
// Bridge installiert nur, was zur Signatur passt (sign.go), und nie von
// selbst: Geprüft wird auf Knopfdruck und einmal am Tag, eingespielt nur auf
// Knopfdruck in der Oberfläche.
//
// Einspielen heißt: neues Binary neben das laufende schreiben, das laufende
// in <name>.old umbenennen, das neue an seine Stelle setzen, Dienst neu
// starten. Das geht auch unter Windows, wo eine laufende .exe nicht
// überschrieben, aber umbenannt werden darf. Die Konfiguration wird nicht
// angefasst. <name>.old bleibt für den Weg zurück liegen.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRepo ist das GitHub-Repo, aus dessen Releases die Updates kommen.
	DefaultRepo = "dade000/erpnext-hardware-bridge"
	defaultAPI  = "https://api.github.com"
	binaryName  = "erpnext-hardware-bridge"
	// Ein Binary hat rund 10 MB; die Grenze fängt nur Unsinn ab.
	maxBinaryBytes = 200 << 20
	maxSmallBytes  = 1 << 20
)

// Info ist der Stand, den die Oberfläche zeigt.
type Info struct {
	Current     string    `json:"current"`
	Latest      string    `json:"latest,omitempty"`
	Available   bool      `json:"available"`
	Notes       string    `json:"notes,omitempty"`
	PublishedAt string    `json:"published_at,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	Error       string    `json:"error,omitempty"`
	// CanInstall: Die Bridge darf ihr eigenes Binary ersetzen. Sonst sagt
	// InstallHint, warum nicht.
	CanInstall  bool   `json:"can_install"`
	InstallHint string `json:"install_hint,omitempty"`
	// Previous: Es liegt eine vorige Version für den Weg zurück bereit.
	Previous bool `json:"previous"`
	// Development: selbst gebaute Version ohne Versionsnummer.
	Development bool `json:"development"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	Tag         string  `json:"tag_name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	Assets      []asset `json:"assets"`
}

// Updater kennt die laufende Version und das Repo.
type Updater struct {
	Repo      string
	APIBase   string
	Current   string
	PublicKey ed25519.PublicKey
	Client    *http.Client
	GOOS      string
	GOARCH    string
	// ExePath liefert den Pfad des laufenden Binarys; Tests ersetzen ihn.
	ExePath func() (string, error)

	mu      sync.Mutex
	latest  *release
	checked time.Time
	lastErr string
	busy    bool
	// Ob der Programmordner beschreibbar ist, wird einmal festgestellt und
	// bei jeder Prüfung aufgefrischt – die Statusseite fragt jede Sekunde.
	probed   bool
	readOnly bool
}

// New baut den Updater für die laufende Bridge.
func New(current string) *Updater {
	key, _ := ParsePublicKey(PublicKey)
	api := defaultAPI
	// Zum Testen des ganzen Ablaufs gegen einen eigenen Server. Die Signatur
	// wird weiterhin gegen den einkompilierten Schlüssel geprüft.
	if v := os.Getenv("ERPNEXT_BRIDGE_UPDATE_API"); v != "" {
		api = v
	}
	return &Updater{
		Repo:      DefaultRepo,
		APIBase:   api,
		Current:   current,
		PublicKey: key,
		Client:    &http.Client{Timeout: 5 * time.Minute},
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		ExePath:   executable,
	}
}

func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Status liefert den zuletzt bekannten Stand, ohne das Netz zu fragen.
func (u *Updater) Status() Info {
	u.mu.Lock()
	latest, checked, lastErr := u.latest, u.checked, u.lastErr
	u.mu.Unlock()

	info := Info{Current: u.Current, CheckedAt: checked, Error: lastErr}
	_, _, _, ok := parseVersion(u.Current)
	info.Development = !ok
	if latest != nil {
		info.Latest = latest.Tag
		info.Notes = latest.Body
		info.PublishedAt = latest.PublishedAt
		info.Available = newer(latest.Tag, u.Current)
	}
	exe, err := u.ExePath()
	if err != nil {
		info.InstallHint = "Der Pfad des Programms lässt sich nicht bestimmen: " + err.Error()
		return info
	}
	if _, err := os.Stat(exe + ".old"); err == nil {
		info.Previous = true
	}
	u.mu.Lock()
	if !u.probed {
		u.probed, u.readOnly = true, writable(filepath.Dir(exe)) != nil
	}
	readOnly := u.readOnly
	u.mu.Unlock()
	if readOnly {
		info.InstallHint = "Der Dienst darf im Programmordner " + filepath.Dir(exe) +
			" nicht schreiben. Das Update dann von Hand einspielen oder die Bridge mit dem Installationsskript neu einrichten."
		return info
	}
	info.CanInstall = true
	return info
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// Check fragt das neueste Release ab.
func (u *Updater) Check(ctx context.Context) (Info, error) {
	rel, err := u.fetchLatest(ctx)
	u.mu.Lock()
	u.checked = time.Now()
	u.probed = false
	if err != nil {
		u.lastErr = err.Error()
	} else {
		u.lastErr = ""
		u.latest = rel
	}
	u.mu.Unlock()
	return u.Status(), err
}

func (u *Updater) fetchLatest(ctx context.Context) (*release, error) {
	body, status, err := u.get(ctx, strings.TrimRight(u.APIBase, "/")+"/repos/"+u.Repo+"/releases/latest", maxSmallBytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub nicht erreichbar: %w", err)
	}
	switch {
	case status == http.StatusNotFound:
		return nil, errors.New("kein Release gefunden (Repo privat oder noch keines veröffentlicht)")
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return nil, errors.New("GitHub lässt gerade keine Abfrage zu (zu viele Anfragen); später erneut versuchen")
	case status != http.StatusOK:
		return nil, fmt.Errorf("GitHub antwortet mit HTTP %d", status)
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil || rel.Tag == "" {
		return nil, errors.New("Antwort von GitHub nicht verstanden")
	}
	return &rel, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "erpnext-hardware-bridge/"+u.Current)
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, errors.New("Antwort ist größer als erwartet")
	}
	return body, resp.StatusCode, nil
}

// AssetName ist der Dateiname des Binarys für diese Plattform im Release.
func (u *Updater) AssetName(tag string) string {
	name := binaryName + "-" + tag + "-" + u.GOOS + "-" + u.GOARCH
	if u.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Install lädt das neueste Release, prüft es und setzt es an die Stelle des
// laufenden Binarys. Der Aufrufer startet danach den Dienst neu.
func (u *Updater) Install(ctx context.Context) (string, error) {
	if !u.begin() {
		return "", errors.New("es läuft bereits ein Update")
	}
	defer u.end()

	if _, err := u.Check(ctx); err != nil {
		return "", err
	}
	u.mu.Lock()
	rel := u.latest
	u.mu.Unlock()
	if !newer(rel.Tag, u.Current) {
		return "", fmt.Errorf("%s ist nicht neuer als die laufende Version %s", rel.Tag, u.Current)
	}

	data, err := u.download(ctx, rel)
	if err != nil {
		return "", err
	}
	exe, err := u.ExePath()
	if err != nil {
		return "", err
	}
	if err := swap(exe, data); err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// download holt das Binary dieser Plattform und prüft Signatur und Prüfsumme.
func (u *Updater) download(ctx context.Context, rel *release) ([]byte, error) {
	if len(u.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("dieser Bridge fehlt der Schlüssel zum Prüfen von Releases")
	}
	// Die Namen tragen die Version: Eine alte, einmal gültig signierte
	// Prüfsummen-Datei lässt sich so nicht einem neuen Release unterschieben.
	sumsName := "SHA256SUMS-" + rel.Tag
	binName := u.AssetName(rel.Tag)
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	for _, need := range []string{sumsName, sumsName + ".sig", binName} {
		if urls[need] == "" {
			if need == binName {
				return nil, fmt.Errorf("Release %s enthält kein Binary für %s/%s", rel.Tag, u.GOOS, u.GOARCH)
			}
			return nil, fmt.Errorf("Release %s ist nicht signiert (%s fehlt)", rel.Tag, need)
		}
	}

	sums, err := u.fetch(ctx, urls[sumsName], maxSmallBytes)
	if err != nil {
		return nil, fmt.Errorf("Prüfsummen: %w", err)
	}
	sig, err := u.fetch(ctx, urls[sumsName+".sig"], maxSmallBytes)
	if err != nil {
		return nil, fmt.Errorf("Signatur: %w", err)
	}
	if err := Verify(u.PublicKey, sums, string(sig)); err != nil {
		return nil, err
	}
	want, err := checksumFor(sums, binName)
	if err != nil {
		return nil, err
	}
	data, err := u.fetch(ctx, urls[binName], maxBinaryBytes)
	if err != nil {
		return nil, fmt.Errorf("Download: %w", err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, errors.New("Prüfsumme des heruntergeladenen Binarys stimmt nicht")
	}
	return data, nil
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, status, err := u.get(ctx, url, limit)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	return body, nil
}

// checksumFor sucht die Zeile "<sha256>  <name>" (Format von sha256sum).
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("Prüfsummen-Datei nennt %s nicht", name)
}

// swap setzt data an die Stelle von exe und hebt die bisherige Fassung als
// exe.old auf.
func swap(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return fmt.Errorf("Programmordner nicht beschreibbar: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("laufendes Programm lässt sich nicht beiseitelegen: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		// Zurück, damit der Dienst beim nächsten Start nicht ohne Programm dasteht.
		_ = os.Rename(old, exe)
		return fmt.Errorf("neues Programm lässt sich nicht einsetzen: %w", err)
	}
	return nil
}

// Rollback tauscht das laufende Binary gegen die vorige Fassung (exe.old).
// Die laufende Fassung liegt danach als exe.old bereit, der Weg führt also
// auch wieder vor.
func (u *Updater) Rollback() error {
	if !u.begin() {
		return errors.New("es läuft bereits ein Update")
	}
	defer u.end()
	exe, err := u.ExePath()
	if err != nil {
		return err
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		return errors.New("es liegt keine vorige Version bereit")
	}
	swapName := exe + ".swap"
	_ = os.Remove(swapName)
	if err := os.Rename(exe, swapName); err != nil {
		return fmt.Errorf("laufendes Programm lässt sich nicht beiseitelegen: %w", err)
	}
	if err := os.Rename(old, exe); err != nil {
		_ = os.Rename(swapName, exe)
		return fmt.Errorf("vorige Version lässt sich nicht einsetzen: %w", err)
	}
	return os.Rename(swapName, old)
}

func (u *Updater) begin() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy {
		return false
	}
	u.busy = true
	return true
}

func (u *Updater) end() {
	u.mu.Lock()
	u.busy = false
	u.mu.Unlock()
}

// parseVersion liest "v1.2.3" (auch mit Anhang wie "-3-gabc123").
func parseVersion(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	n := make([]int, 3)
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return 0, 0, 0, false
		}
		n[i] = x
	}
	return n[0], n[1], n[2], true
}

// newer: Ist candidate neuer als current? Eine selbst gebaute Version ohne
// Nummer gilt als älter als jedes Release.
func newer(candidate, current string) bool {
	a1, a2, a3, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	b1, b2, b3, ok := parseVersion(current)
	if !ok {
		return true
	}
	if a1 != b1 {
		return a1 > b1
	}
	if a2 != b2 {
		return a2 > b2
	}
	return a3 > b3
}
