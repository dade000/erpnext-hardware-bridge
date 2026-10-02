package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub liefert ein Release mit Binary, Prüfsummen und Signatur.
type fakeGitHub struct {
	mu     sync.Mutex
	tag    string
	files  map[string][]byte
	status int
	srv    *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{files: map[string][]byte{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			if g.status != 0 {
				w.WriteHeader(g.status)
				return
			}
			var assets []asset
			for name := range g.files {
				assets = append(assets, asset{Name: name, URL: g.srv.URL + "/dl/" + name})
			}
			_ = json.NewEncoder(w).Encode(release{Tag: g.tag, Body: "Druckmodul", PublishedAt: "2026-10-02T10:00:00Z", Assets: assets})
			return
		}
		data, ok := g.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// publish legt ein vollständiges, mit key signiertes Release an.
func (g *fakeGitHub) publish(tag string, binary []byte, key ed25519.PrivateKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tag = tag
	g.files = map[string][]byte{}
	bin := "erpnext-hardware-bridge-" + tag + "-linux-amd64"
	sum := sha256.Sum256(binary)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + bin + "\n" +
		strings.Repeat("0", 64) + "  erpnext-hardware-bridge-" + tag + "-windows-amd64.exe\n")
	g.files[bin] = binary
	g.files["SHA256SUMS-"+tag] = sums
	g.files["SHA256SUMS-"+tag+".sig"] = []byte(Sign(key, sums) + "\n")
}

type env struct {
	u   *Updater
	gh  *fakeGitHub
	key ed25519.PrivateKey
	exe string
}

func setup(t *testing.T, current string) *env {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	exe := filepath.Join(t.TempDir(), "erpnext-hardware-bridge")
	if err := os.WriteFile(exe, []byte("laufende Fassung"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := New(current)
	u.APIBase, u.PublicKey, u.GOOS, u.GOARCH = gh.srv.URL, pub, "linux", "amd64"
	u.ExePath = func() (string, error) { return exe, nil }
	return &env{u: u, gh: gh, key: priv, exe: exe}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckReportsANewerRelease(t *testing.T) {
	e := setup(t, "v0.1.2")
	e.gh.publish("v0.1.3", []byte("neu"), e.key)
	info, err := e.u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Available || info.Latest != "v0.1.3" || info.Notes != "Druckmodul" || !info.CanInstall || info.Previous {
		t.Fatalf("%+v", info)
	}
	// Gleiche und ältere Versionen sind kein Update.
	for _, tag := range []string{"v0.1.2", "v0.1.1", "v0.0.9"} {
		e.gh.publish(tag, []byte("x"), e.key)
		if info, _ := e.u.Check(context.Background()); info.Available {
			t.Fatalf("%s gilt als Update für v0.1.2", tag)
		}
	}
}

func TestVersionComparison(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"v0.1.3", "v0.1.2", true},
		{"v0.2.0", "v0.1.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.10", "v0.1.9", true},
		{"v0.1.2", "v0.1.2", false},
		{"v0.1.2", "v0.1.2-3-gabc123", false}, // Zwischenstand nach v0.1.2
		{"v0.1.3", "dev", true},               // selbst gebaut: jedes Release ist neuer
		{"unsinn", "v0.1.2", false},
	} {
		if got := newer(c.candidate, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.candidate, c.current, got)
		}
	}
}

func TestInstallReplacesTheBinaryAndKeepsTheOldOne(t *testing.T) {
	e := setup(t, "v0.1.2")
	e.gh.publish("v0.1.3", []byte("neue Fassung"), e.key)

	tag, err := e.u.Install(context.Background())
	if err != nil || tag != "v0.1.3" {
		t.Fatalf("Install: %q %v", tag, err)
	}
	if read(t, e.exe) != "neue Fassung" || read(t, e.exe+".old") != "laufende Fassung" {
		t.Fatalf("exe=%q old=%q", read(t, e.exe), read(t, e.exe+".old"))
	}
	if st, _ := os.Stat(e.exe); st.Mode().Perm()&0o111 == 0 {
		t.Fatal("neues Binary ist nicht ausführbar")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(e.exe), ".update-*")); len(left) != 0 {
		t.Fatalf("Reste im Programmordner: %v", left)
	}
	if !e.u.Status().Previous {
		t.Fatal("vorige Version wird nicht angeboten")
	}

	// Und zurück – und wieder vor.
	if err := e.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	if read(t, e.exe) != "laufende Fassung" || read(t, e.exe+".old") != "neue Fassung" {
		t.Fatalf("nach Rollback: exe=%q old=%q", read(t, e.exe), read(t, e.exe+".old"))
	}
}

// Alles, was nicht aus dem eigenen Release-Lauf stammt, bleibt draußen – und
// das laufende Binary bleibt unberührt.
func TestInstallRefusesWhatItCannotTrust(t *testing.T) {
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func(e *env){
		"fremder Schlüssel": func(e *env) { e.gh.publish("v0.1.3", []byte("böse"), stranger) },
		"Binary nach dem Signieren getauscht": func(e *env) {
			e.gh.publish("v0.1.3", []byte("gut"), e.key)
			e.gh.files["erpnext-hardware-bridge-v0.1.3-linux-amd64"] = []byte("böse")
		},
		"Prüfsummen nach dem Signieren geändert": func(e *env) {
			e.gh.publish("v0.1.3", []byte("gut"), e.key)
			e.gh.files["SHA256SUMS-v0.1.3"] = append(e.gh.files["SHA256SUMS-v0.1.3"], '\n')
		},
		"Signatur fehlt": func(e *env) {
			e.gh.publish("v0.1.3", []byte("gut"), e.key)
			delete(e.gh.files, "SHA256SUMS-v0.1.3.sig")
		},
		"altes Release unter neuem Namen": func(e *env) {
			// Gültig signierte Dateien von v0.1.1, als v0.1.3 ausgegeben.
			e.gh.publish("v0.1.1", []byte("alt"), e.key)
			e.gh.tag = "v0.1.3"
		},
		"kein Binary für diese Plattform": func(e *env) {
			e.gh.publish("v0.1.3", []byte("gut"), e.key)
			delete(e.gh.files, "erpnext-hardware-bridge-v0.1.3-linux-amd64")
		},
		"nicht neuer": func(e *env) { e.gh.publish("v0.1.2", []byte("gleich"), e.key) },
	}
	for name, prepare := range cases {
		e := setup(t, "v0.1.2")
		prepare(e)
		if _, err := e.u.Install(context.Background()); err == nil {
			t.Errorf("%s: Install hätte scheitern müssen", name)
		}
		if read(t, e.exe) != "laufende Fassung" {
			t.Errorf("%s: laufendes Binary verändert", name)
		}
		if _, err := os.Stat(e.exe + ".old"); err == nil {
			t.Errorf("%s: .old angelegt", name)
		}
	}
}

func TestCheckErrorsAreKeptForTheStatus(t *testing.T) {
	e := setup(t, "v0.1.2")
	e.gh.status = http.StatusNotFound
	info, err := e.u.Check(context.Background())
	if err == nil || !strings.Contains(info.Error, "kein Release") {
		t.Fatalf("%v %+v", err, info)
	}
	if e.u.Status().Error == "" {
		t.Fatal("Fehler steht nicht im Status")
	}
}

func TestStatusExplainsAReadOnlyProgramFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root darf überall schreiben")
	}
	e := setup(t, "v0.1.2")
	dir := filepath.Dir(e.exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if info := e.u.Status(); info.CanInstall || info.InstallHint == "" {
		t.Fatalf("%+v", info)
	}
}

func TestRollbackWithoutAPreviousVersion(t *testing.T) {
	e := setup(t, "v0.1.2")
	if err := e.u.Rollback(); err == nil {
		t.Fatal("Rollback ohne .old muss scheitern")
	}
	if read(t, e.exe) != "laufende Fassung" {
		t.Fatal("laufendes Binary verändert")
	}
}

func TestAssetNames(t *testing.T) {
	u := New("v0.1.2")
	for _, c := range []struct{ os, arch, want string }{
		{"linux", "amd64", "erpnext-hardware-bridge-v0.2.0-linux-amd64"},
		{"linux", "arm", "erpnext-hardware-bridge-v0.2.0-linux-arm"},
		{"windows", "amd64", "erpnext-hardware-bridge-v0.2.0-windows-amd64.exe"},
	} {
		u.GOOS, u.GOARCH = c.os, c.arch
		if got := u.AssetName("v0.2.0"); got != c.want {
			t.Errorf("%s/%s: %s", c.os, c.arch, got)
		}
	}
}

// Der einkompilierte Schlüssel muss ein gültiger Ed25519-Schlüssel sein,
// sonst kann keine ausgelieferte Bridge je ein Update annehmen.
func TestEmbeddedPublicKeyIsValid(t *testing.T) {
	if _, err := ParsePublicKey(PublicKey); err != nil {
		t.Fatal(err)
	}
}

func TestSignAndVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	data := []byte("abc  datei\n")
	sig := Sign(priv, data)
	if err := Verify(pub, data, sig+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, []byte("abd  datei\n"), sig); err == nil {
		t.Fatal("geänderte Daten angenommen")
	}
	if err := Verify(pub, data, "kein base64"); err == nil {
		t.Fatal("kaputte Signatur angenommen")
	}
	// Seed und voller Schlüssel ergeben denselben Schlüssel.
	seedKey, err := ParsePrivateKey(encode(priv.Seed()))
	if err != nil || !seedKey.Equal(priv) {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := ParsePrivateKey("AAAA"); err == nil {
		t.Fatal("zu kurzer Schlüssel angenommen")
	}
}
