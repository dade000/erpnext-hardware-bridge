package main

import "testing"

func TestServiceDependenciesOnlyOnLinux(t *testing.T) {
	if d := serviceConfig("windows", nil, "").Dependencies; len(d) != 0 {
		t.Fatalf("Windows-Dienst darf keine systemd-Abhängigkeiten tragen: %v", d)
	}
	if d := serviceConfig("linux", nil, "").Dependencies; len(d) != 1 || d[0] != "After=network.target" {
		t.Fatalf("Linux: %v", d)
	}
}
