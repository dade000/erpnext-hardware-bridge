//go:build !linux

package streamdeck

import "errors"

// OpenHID: der Zugriff über hidraw gibt es nur unter Linux.
func OpenHID(string) (Conn, Info, error) {
	return nil, Info{}, errors.New("Stream Deck wird bisher nur unter Linux unterstützt")
}
