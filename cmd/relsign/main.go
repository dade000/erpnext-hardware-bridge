// relsign erzeugt das Schlüsselpaar für signierte Releases und unterschreibt
// die Prüfsummen-Datei eines Releases.
//
//	relsign keygen <datei>     Schlüsselpaar erzeugen: privater Schlüssel in die
//	                           Datei, öffentlicher Schlüssel auf die Konsole
//	relsign sign <datei>       <datei>.sig schreiben; der private Schlüssel
//	                           kommt aus der Umgebung (RELEASE_SIGNING_KEY)
//	relsign verify <datei>     <datei>.sig gegen den einkompilierten
//	                           öffentlichen Schlüssel prüfen
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"erpnext-hardware-bridge/internal/update"
)

func main() {
	if len(os.Args) != 3 {
		fail("Aufruf: relsign keygen|sign|verify <datei>")
	}
	cmd, path := os.Args[1], os.Args[2]
	switch cmd {
	case "keygen":
		if _, err := os.Stat(path); err == nil {
			fail(path + " gibt es schon – ein bestehender Schlüssel wird nicht überschrieben")
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		check(os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600))
		fmt.Println("Privater Schlüssel:", path, "(als GitHub-Secret RELEASE_SIGNING_KEY hinterlegen, sicher aufbewahren)")
		fmt.Println("Öffentlicher Schlüssel für internal/update/pubkey.go:")
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
	case "sign":
		key, err := update.ParsePrivateKey(os.Getenv("RELEASE_SIGNING_KEY"))
		if err != nil {
			fail("RELEASE_SIGNING_KEY: " + err.Error())
		}
		data, err := os.ReadFile(path)
		check(err)
		sig := update.Sign(key, data)
		// Nie ein Release unterschreiben, das die ausgelieferten Bridges
		// ablehnen würden (falscher Schlüssel im Secret).
		pub, err := update.ParsePublicKey(update.PublicKey)
		check(err)
		if err := update.Verify(pub, data, sig); err != nil {
			fail("Der Schlüssel in RELEASE_SIGNING_KEY passt nicht zum öffentlichen Schlüssel in pubkey.go")
		}
		check(os.WriteFile(path+".sig", []byte(sig+"\n"), 0o644))
		fmt.Println(path + ".sig")
	case "verify":
		pub, err := update.ParsePublicKey(update.PublicKey)
		check(err)
		data, err := os.ReadFile(path)
		check(err)
		sig, err := os.ReadFile(path + ".sig")
		check(err)
		check(update.Verify(pub, data, string(sig)))
		fmt.Println("Signatur in Ordnung")
	default:
		fail("unbekannter Befehl " + cmd)
	}
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "relsign:", msg)
	os.Exit(1)
}
