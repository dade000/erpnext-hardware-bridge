// Command bridge ist die ERPNext Hardware Bridge.
//
//	bridge                           im Vordergrund starten
//	bridge -config ./bridge.yaml     mit eigener Konfigurationsdatei
//	bridge -service install          als Dienst einrichten (systemd / Windows-Dienst)
//	bridge -service uninstall|start|stop|restart|status
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"erpnext-hardware-bridge/internal/app"
	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/logbuf"
	"erpnext-hardware-bridge/internal/version"
)

const serviceName = "erpnext-hardware-bridge"

type program struct {
	cfgPath string
	logPath string
	app     *app.App
	file    *logbuf.RotatingFile
	log     *slog.Logger
}

func (p *program) Start(service.Service) error {
	ring := logbuf.NewRing(500)
	var extra []*logbuf.RotatingFile
	if p.logPath != "" {
		if err := os.MkdirAll(filepath.Dir(p.logPath), 0o750); err == nil {
			if f, err := logbuf.OpenRotating(p.logPath, 2<<20); err == nil {
				p.file = f
				extra = append(extra, f)
			} else {
				fmt.Fprintln(os.Stderr, "Logdatei:", err)
			}
		}
	}
	var log *slog.Logger
	var level *slog.LevelVar
	if len(extra) > 0 {
		log, level = logbuf.Setup(ring, extra[0])
	} else {
		log, level = logbuf.Setup(ring)
	}
	p.log = log

	cfg, exists, err := config.Load(p.cfgPath)
	if err != nil {
		log.Error("Konfiguration kann nicht geladen werden", "path", p.cfgPath, "err", err)
		return err
	}
	log.Info("ERPNext Hardware Bridge startet", "version", version.Version, "config", p.cfgPath, "exists", exists)
	p.app = app.New(p.cfgPath, cfg, exists, log, level, ring)
	return p.app.Start()
}

func (p *program) Stop(service.Service) error {
	if p.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		p.app.Shutdown(ctx)
		cancel()
		p.log.Info("Bridge beendet")
	}
	if p.file != nil {
		_ = p.file.Close()
	}
	return nil
}

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "Pfad zu bridge.yaml")
	logPath := flag.String("log-file", "", "Logdatei (Standard: bridge.log neben der Konfiguration, \"-\" = keine)")
	svcCmd := flag.String("service", "", "install | uninstall | start | stop | restart | status")
	user := flag.String("user", "", "Dienstbenutzer bei -service install (Linux; braucht Gruppe dialout)")
	showVersion := flag.Bool("version", false, "Version ausgeben")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	abs, err := filepath.Abs(*cfgPath)
	if err == nil {
		*cfgPath = abs
	}
	lp := *logPath
	switch lp {
	case "":
		lp = filepath.Join(filepath.Dir(*cfgPath), "bridge.log")
	case "-":
		lp = ""
	}

	prg := &program{cfgPath: *cfgPath, logPath: lp}
	args := []string{"-config", *cfgPath}
	if *logPath != "" {
		args = append(args, "-log-file", *logPath)
	}
	svc, err := service.New(prg, &service.Config{
		Name:        serviceName,
		DisplayName: "ERPNext Hardware Bridge",
		Description: "Stellt Waage, Kamera, Scanner und Terminal dem ERPNext-Desk per lokalem WebSocket bereit.",
		Arguments:   args,
		UserName:    *user,
		Dependencies: []string{
			"After=network.target",
		},
		Option: service.KeyValue{
			"Restart":          "always",
			"OnFailure":        "restart", // Windows
			"DelayedAutoStart": false,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *svcCmd != "" {
		if *svcCmd == "status" {
			st, err := svc.Status()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(map[service.Status]string{service.StatusRunning: "läuft", service.StatusStopped: "gestoppt"}[st])
			return
		}
		if err := service.Control(svc, *svcCmd); err != nil {
			fmt.Fprintf(os.Stderr, "service %s: %v\n", *svcCmd, err)
			os.Exit(1)
		}
		fmt.Printf("service %s: ok\n", *svcCmd)
		return
	}

	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
