package update

// PublicKey ist der öffentliche Ed25519-Schlüssel (Base64), mit dem die
// Bridge Releases prüft. Der private Gegenschlüssel liegt nur als
// GitHub-Secret RELEASE_SIGNING_KEY vor. Neues Paar: `go run ./cmd/relsign
// keygen <datei>` – danach nehmen ausgelieferte Bridges keine Updates mehr an,
// bis sie einmal von Hand auf eine Version mit dem neuen Schlüssel gebracht
// wurden.
const PublicKey = "Nq7E05DHcIqKgRXL+8UPLTi4jMjntfhNIOW1DiRSNqA="
