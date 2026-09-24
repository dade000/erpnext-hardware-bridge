// Package netutil enthält die Netz-Grundregeln der Bridge: Loopback-Listener
// für IPv4 und IPv6 sowie die Prüfung von Host- und Origin-Header.
package netutil

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Listener ist ein gebundener Socket mit seiner Adressfamilie.
type Listener struct {
	net.Listener
	Family string // "ipv4" oder "ipv6"
}

// ListenLoopback bindet den Port auf 127.0.0.1 und ::1. Fehlt eine Familie
// (IPv6 abgeschaltet), läuft die Bridge mit der anderen weiter; die Warnung
// kommt als zweiter Rückgabewert. Nur wenn beide scheitern, gibt es einen Fehler.
func ListenLoopback(port int) ([]Listener, []string, error) {
	var ls []Listener
	var warns []string
	var errs []error
	for _, a := range []struct{ fam, addr string }{
		{"ipv4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		{"ipv6", net.JoinHostPort("::1", strconv.Itoa(port))},
	} {
		l, err := net.Listen("tcp", a.addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.addr, err))
			warns = append(warns, fmt.Sprintf("%s nicht gebunden: %v", a.addr, err))
			continue
		}
		ls = append(ls, Listener{Listener: l, Family: a.fam})
	}
	if len(ls) == 0 {
		return nil, nil, errors.Join(errs...)
	}
	return ls, warns, nil
}

// loopbackNames sind die Hostnamen, unter denen die Oberfläche und die
// WS-API erreichbar sein dürfen. Alles andere im Host-Header deutet auf
// DNS-Rebinding hin (fremde Domain, die auf 127.0.0.1 zeigt).
var loopbackNames = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// IsLoopbackHost prüft den Host-Header gegen localhost/127.0.0.1/[::1] mit
// dem erwarteten Port.
func IsLoopbackHost(hostHeader string, port int) bool {
	h, p, err := net.SplitHostPort(hostHeader)
	if err != nil {
		return false
	}
	return loopbackNames[strings.ToLower(h)] && p == strconv.Itoa(port)
}

// IsLoopbackOrigin prüft, ob ein Origin die eigene Oberfläche ist.
func IsLoopbackOrigin(origin string, port int) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return IsLoopbackHost(u.Host, port)
}

// OriginAllowed vergleicht exakt (Schema, Host, Port) und ohne
// Groß-/Kleinschreibung. Ein fehlender Origin ist nicht erlaubt.
func OriginAllowed(origin string, allowed []string) bool {
	o := strings.TrimRight(strings.ToLower(strings.TrimSpace(origin)), "/")
	if o == "" || o == "null" {
		return false
	}
	for _, a := range allowed {
		if o == a {
			return true
		}
	}
	return false
}

// IsLoopbackBind meldet, ob eine Listen-Adresse nur lokal erreichbar ist.
func IsLoopbackBind(listen string) bool {
	h, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
