//go:build !linux

package app

import "erpnext-hardware-bridge/internal/device"

// registerOptional: libgphoto2 gibt es nur unter Linux. Ein konfiguriertes
// gphoto2-Gerät meldet hier »Treiber nicht enthalten«.
func registerOptional(*device.Manager) {}
