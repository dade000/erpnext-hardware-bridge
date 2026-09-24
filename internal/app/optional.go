package app

import "erpnext-hardware-bridge/internal/device"

// registerOptional registriert Treiber, die nur in bestimmten Builds
// enthalten sind. Die Kamera (libgphoto2, nur Linux, Build-Tag "camera")
// kommt in Phase 2 hinzu; bis dahin meldet ein konfiguriertes Kameragerät
// »Treiber nicht enthalten« und die Kompat-Route reicht /shot an den
// Legacy-Upstream weiter.
func registerOptional(*device.Manager) {}
