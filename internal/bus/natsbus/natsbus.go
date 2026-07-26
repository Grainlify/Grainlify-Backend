package natsbus

import (
	"context"
	"fmt"
	"log/slog"
	neturl "net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	natsClientName          = "grainlify-api"
	natsConnectTimeout      = 5 * time.Second
	natsMaxReconnects       = 120
	natsReconnectWait       = time.Second
	natsReconnectJitter     = 250 * time.Millisecond
	natsReconnectJitterTLS  = time.Second
	natsReconnectBufferSize = 8 * 1024 * 1024
	natsMaskedCredential    = "redacted"
)

type Bus struct {
	nc *nats.Conn
}

func Connect(url string) (*Bus, error) {
	if url == "" {
		return nil, fmt.Errorf("NATS_URL is required")
	}

	// Mask URL for logging (don't expose credentials)
	maskedURL := maskNATSURL(url)
	slog.Info("connecting to NATS",
		"url_masked", maskedURL,
		"timeout", natsConnectTimeout,
		"max_reconnects", natsMaxReconnects,
		"reconnect_wait", natsReconnectWait,
	)

	nc, err := nats.Connect(url, natsConnectOptions()...)
	if err != nil {
		slog.Error("failed to connect to NATS",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		return nil, err
	}

	slog.Info("NATS connection established",
		"status", nc.Status().String(),
		"connected_url_masked", maskNATSURL(nc.ConnectedUrl()),
	)

	return &Bus{nc: nc}, nil
}

// natsConnectOptions centralises connection behavior so reconnect policy is
// explicit and unit-tested instead of relying on nats.go defaults.
// Core NATS subscriptions are owned by the connection; with reconnect enabled,
// nats.go replays active subscriptions after a successful reconnect.
func natsConnectOptions() []nats.Option {
	return []nats.Option{
		func(o *nats.Options) error {
			o.AllowReconnect = true
			return nil
		},
		func(o *nats.Options) error {
			o.NoCallbacksAfterClientClose = true
			return nil
		},
		nats.Name(natsClientName),
		nats.Timeout(natsConnectTimeout),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(natsMaxReconnects),
		nats.ReconnectWait(natsReconnectWait),
		nats.ReconnectJitter(natsReconnectJitter, natsReconnectJitterTLS),
		nats.ReconnectBufSize(natsReconnectBufferSize),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			logNATSConnectionTransition(slog.LevelWarn, "NATS connection disconnected", "disconnected", nc, err)
		}),
		nats.ReconnectErrHandler(func(nc *nats.Conn, err error) {
			logNATSConnectionTransition(slog.LevelWarn, "NATS reconnect attempt failed", "reconnect_failed", nc, err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logNATSConnectionTransition(slog.LevelInfo, "NATS connection reconnected", "reconnected", nc, nil)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logNATSConnectionTransition(slog.LevelError, "NATS connection closed", "closed", nc, nil)
		}),
	}
}

func logNATSConnectionTransition(level slog.Level, msg, transition string, nc *nats.Conn, err error) {
	attrs := []any{
		"transition", transition,
		"max_reconnects", natsMaxReconnects,
		"reconnect_wait", natsReconnectWait,
	}
	if nc == nil {
		attrs = append(attrs, "status", "UNKNOWN")
	} else {
		stats := nc.Stats()
		attrs = append(attrs,
			"status", nc.Status().String(),
			"connected_url_masked", maskNATSURL(nc.ConnectedUrl()),
			"reconnects", stats.Reconnects,
		)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.Log(context.Background(), level, msg, attrs...)
}

// maskNATSURL masks credentials in NATS URL for logging
func maskNATSURL(rawURL string) string {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "***"
	}
	if u.User == nil {
		return u.String()
	}
	username := u.User.Username()
	if _, hasPassword := u.User.Password(); !hasPassword {
		u.User = neturl.User(natsMaskedCredential)
	} else if username == "" {
		u.User = neturl.UserPassword("", natsMaskedCredential)
	} else {
		u.User = neturl.UserPassword(username, natsMaskedCredential)
	}
	return u.String()
}

func (b *Bus) Publish(ctx context.Context, subject string, data []byte) error {
	if b == nil || b.nc == nil {
		return fmt.Errorf("nats not connected")
	}
	// nats.go Publish is fast; respect ctx only for cancellation before send.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return b.nc.Publish(subject, data)
}

func (b *Bus) Status() string {
	if b == nil || b.nc == nil {
		return "DISCONNECTED"
	}
	return b.nc.Status().String()
}

func (b *Bus) Close() {
	if b == nil || b.nc == nil {
		return
	}
	slog.Info("closing NATS connection")
	_ = b.nc.Drain()
	b.nc.Close()
	slog.Info("NATS connection closed")
}

func (b *Bus) Conn() *nats.Conn { return b.nc }
