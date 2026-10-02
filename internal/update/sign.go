package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
)

// Releases werden mit Ed25519 signiert: Der Release-Workflow unterschreibt
// die Prüfsummen-Datei mit dem privaten Schlüssel (GitHub-Secret
// RELEASE_SIGNING_KEY), die Bridge prüft mit dem öffentlichen Schlüssel, der
// in ihr einkompiliert ist (pubkey.go). So nimmt eine Bridge nur Binaries an,
// die aus dem eigenen Release-Lauf stammen – eine Prüfsumme allein, vom
// selben Ort geladen wie das Binary, würde nur Übertragungsfehler erkennen.

// ParsePublicKey liest einen Base64-kodierten öffentlichen Schlüssel.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("öffentlicher Schlüssel ist kein Ed25519-Schlüssel")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey liest einen Base64-kodierten privaten Schlüssel (64 Byte)
// oder dessen Seed (32 Byte).
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, errors.New("privater Schlüssel ist kein Base64")
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, errors.New("privater Schlüssel hat die falsche Länge")
}

// Sign liefert die Signatur über data, Base64-kodiert.
func Sign(key ed25519.PrivateKey, data []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))
}

// Verify prüft eine Base64-kodierte Signatur über data.
func Verify(key ed25519.PublicKey, data []byte, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("Signatur ist beschädigt")
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, data, sig) {
		return errors.New("Signatur passt nicht – das Release stammt nicht aus dem eigenen Release-Lauf")
	}
	return nil
}
