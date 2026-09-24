package netutil

import (
	"net"
	"testing"
)

func TestLoopbackHost(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost:8735": true, "127.0.0.1:8735": true, "[::1]:8735": true, "LOCALHOST:8735": true,
		"localhost:9999": false, "evil.example:8735": false, "192.168.1.5:8735": false,
		"localhost": false, "127.0.0.2:8735": false,
	} {
		if got := IsLoopbackHost(h, 8735); got != want {
			t.Errorf("%s: %v", h, got)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	allowed := []string{"https://erp.holzschuhe.at"}
	for o, want := range map[string]bool{
		"https://erp.holzschuhe.at": true, "https://ERP.holzschuhe.at/": true,
		"http://erp.holzschuhe.at": false, "https://erp.holzschuhe.at.evil.com": false,
		"": false, "null": false,
	} {
		if got := OriginAllowed(o, allowed); got != want {
			t.Errorf("%q: %v", o, got)
		}
	}
}

func TestListenLoopbackDualStack(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ls, warns, err := ListenLoopback(port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, x := range ls {
			x.Close()
		}
	}()
	fams := map[string]bool{}
	for _, x := range ls {
		fams[x.Family] = true
		if !x.Addr().(*net.TCPAddr).IP.IsLoopback() {
			t.Fatalf("nicht Loopback: %v", x.Addr())
		}
	}
	if !fams["ipv4"] {
		t.Fatal("IPv4 fehlt")
	}
	if !fams["ipv6"] {
		t.Logf("IPv6-Loopback in dieser Umgebung nicht verfügbar: %v", warns)
	}
}

func TestLoopbackBind(t *testing.T) {
	for a, want := range map[string]bool{"127.0.0.1:5000": true, "[::1]:5000": true, "localhost:5000": true, ":5000": false, "0.0.0.0:5000": false, "[::]:5000": false} {
		if got := IsLoopbackBind(a); got != want {
			t.Errorf("%s: %v", a, got)
		}
	}
}
