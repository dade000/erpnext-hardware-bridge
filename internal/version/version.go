// Package version trägt die Build-Version (per -ldflags gesetzt).
package version

// Version wird beim Release-Build gesetzt:
//
//	go build -ldflags "-X erpnext-hardware-bridge/internal/version.Version=v0.1.0"
var Version = "dev"

// Protocol ist die Hauptversion des WebSocket-Protokolls. Clients lehnen
// unbekannte Hauptversionen mit einer klaren Meldung ab.
const Protocol = 1
