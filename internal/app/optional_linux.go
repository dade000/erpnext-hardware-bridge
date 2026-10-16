package app

import (
	"erpnext-hardware-bridge/internal/camera"
	"erpnext-hardware-bridge/internal/camera/gphoto"
	"erpnext-hardware-bridge/internal/device"
)

// registerOptional registriert Treiber, die es nur unter Linux gibt: die
// Kamera über libgphoto2. Die Bibliothek wird erst beim Verbinden geladen;
// fehlt sie, meldet das Gerät das in seinem Zustand.
func registerOptional(m *device.Manager) {
	m.Register(camera.Kind, "gphoto2", camera.New(gphoto.Open))
}
