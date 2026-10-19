package streamdeck

import (
	"errors"
	"sync"
)

// simConn ist ein Stream Deck ohne Hardware: Bilder werden verworfen, Read
// wartet, bis die Verbindung geschlossen wird. Gedrückt wird über
// streamdeck.press (Oberfläche, Tests).
type simConn struct {
	once   sync.Once
	closed chan struct{}
}

func openSimulator(string) (Conn, Info, error) {
	return &simConn{closed: make(chan struct{})}, Info{Path: "simulator", PID: 0x0080, Serial: "SIMULATOR"}, nil
}

func (s *simConn) Read([]byte) (int, error) {
	<-s.closed
	return 0, errors.New("geschlossen")
}

func (s *simConn) Write([]byte) error               { return nil }
func (s *simConn) SetFeature([]byte) error          { return nil }
func (s *simConn) GetFeature(p []byte) (int, error) { return 0, errors.New("simulator") }

func (s *simConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}
