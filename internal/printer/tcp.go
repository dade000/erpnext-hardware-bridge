package printer

import (
	"context"
	"net"
	"time"
)

// tcpSink druckt über Port 9100 (RAW/JetDirect) auf einen Netzwerkdrucker.
type tcpSink struct {
	rawOnly
	addr string
}

func (s *tcpSink) Target() string { return "tcp " + s.addr }

func (s *tcpSink) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	return d.DialContext(ctx, "tcp", s.addr)
}

func (s *tcpSink) Check(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	return c.Close()
}

func (s *tcpSink) Write(ctx context.Context, job Job) error {
	data := job.Data
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetWriteDeadline(dl)
	}
	if _, err := c.Write(data); err != nil {
		return err
	}
	// Sauber schließen, damit der Drucker den Auftrag als vollständig sieht.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	return nil
}
