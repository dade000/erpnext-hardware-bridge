package kardex

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"erpnext-hardware-bridge/internal/config"
	"erpnext-hardware-bridge/internal/device"
	"erpnext-hardware-bridge/internal/lift/kardex/fakec3000"
)

// SimulatorTrays: so viele Träger hat der simulierte Megamat.
const SimulatorTrays = 24

// NewSimulator baut einen Lift ohne Hardware: eine nachgebaute C3000 auf
// einem freien Port von 127.0.0.1, ein Träger fährt in 0,4 s eine Stellung
// weiter. Zum Ausprobieren der Desk-Seite.
func NewSimulator(cfg config.DeviceConfig, bus *device.Bus, log *slog.Logger) device.Device {
	return newLift(cfg, bus, log, "simulator", func(ctx context.Context) (*Client, error) {
		m := fakec3000.New(SimulatorTrays)
		m.Step = 400 * time.Millisecond
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		srv := &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		go func() {
			<-ctx.Done()
			m.Close()
			srv.Close()
		}()
		c := NewClient(ln.Addr().String())
		c.debug = func(msg string, args ...any) { log.Debug(msg, args...) }
		return c, nil
	})
}
