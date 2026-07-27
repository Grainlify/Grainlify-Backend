package natsbus

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestConnectOptionsExplicitReconnectPolicy(t *testing.T) {
	opts := applyNATSConnectOptions(t)

	if !opts.AllowReconnect {
		t.Fatal("AllowReconnect = false, want true")
	}
	if !opts.RetryOnFailedConnect {
		t.Fatal("RetryOnFailedConnect = false, want true")
	}
	if !opts.NoCallbacksAfterClientClose {
		t.Fatal("NoCallbacksAfterClientClose = false, want true")
	}
	if opts.MaxReconnect != natsMaxReconnects {
		t.Fatalf("MaxReconnect = %d, want %d", opts.MaxReconnect, natsMaxReconnects)
	}
	if opts.ReconnectWait != natsReconnectWait {
		t.Fatalf("ReconnectWait = %s, want %s", opts.ReconnectWait, natsReconnectWait)
	}
	if opts.ReconnectJitter != natsReconnectJitter {
		t.Fatalf("ReconnectJitter = %s, want %s", opts.ReconnectJitter, natsReconnectJitter)
	}
	if opts.ReconnectJitterTLS != natsReconnectJitterTLS {
		t.Fatalf("ReconnectJitterTLS = %s, want %s", opts.ReconnectJitterTLS, natsReconnectJitterTLS)
	}
	if opts.ReconnectBufSize != natsReconnectBufferSize {
		t.Fatalf("ReconnectBufSize = %d, want %d", opts.ReconnectBufSize, natsReconnectBufferSize)
	}
	if opts.Timeout != natsConnectTimeout {
		t.Fatalf("Timeout = %s, want %s", opts.Timeout, natsConnectTimeout)
	}
	if opts.Name != natsClientName {
		t.Fatalf("Name = %q, want %q", opts.Name, natsClientName)
	}
	if opts.DisconnectedErrCB == nil {
		t.Fatal("DisconnectedErrCB is nil")
	}
	if opts.ReconnectedCB == nil {
		t.Fatal("ReconnectedCB is nil")
	}
	if opts.ReconnectErrCB == nil {
		t.Fatal("ReconnectErrCB is nil")
	}
	if opts.ClosedCB == nil {
		t.Fatal("ClosedCB is nil")
	}
}

func TestConnectCallbacksLogStateTransitions(t *testing.T) {
	opts := applyNATSConnectOptions(t)

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	opts.DisconnectedErrCB(nil, errors.New("broker restarted"))
	opts.ReconnectErrCB(nil, errors.New("dial tcp refused"))
	opts.ReconnectedCB(nil)
	opts.ClosedCB(nil)

	got := buf.String()
	for _, want := range []string{
		"NATS connection disconnected",
		"transition=disconnected",
		"error=\"broker restarted\"",
		"NATS reconnect attempt failed",
		"transition=reconnect_failed",
		"error=\"dial tcp refused\"",
		"NATS connection reconnected",
		"transition=reconnected",
		"NATS connection closed",
		"transition=closed",
		"max_reconnects=120",
		"reconnect_wait=1s",
		"status=UNKNOWN",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log output missing %q:\n%s", want, got)
		}
	}
}

func TestMaskNATSURLRedactsCredentials(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "username and password",
			in:   "nats://user:secret@example.test:4222",
			want: "nats://user:redacted@example.test:4222",
		},
		{
			name: "token",
			in:   "nats://token@example.test:4222",
			want: "nats://redacted@example.test:4222",
		},
		{
			name: "no credentials",
			in:   "nats://example.test:4222",
			want: "nats://example.test:4222",
		},
		{
			name: "invalid",
			in:   "not a nats url",
			want: "***",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskNATSURL(tt.in); got != tt.want {
				t.Fatalf("maskNATSURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestReconnectConstantsStayWithinExpectedOperationalWindow(t *testing.T) {
	total := time.Duration(natsMaxReconnects) * natsReconnectWait
	if total < time.Minute {
		t.Fatalf("reconnect window = %s, want at least 1m", total)
	}
	if total > 5*time.Minute {
		t.Fatalf("reconnect window = %s, want at most 5m", total)
	}
}

func applyNATSConnectOptions(t *testing.T) nats.Options {
	t.Helper()

	opts := nats.GetDefaultOptions()
	for _, opt := range natsConnectOptions() {
		if err := opt(&opts); err != nil {
			t.Fatalf("apply NATS option: %v", err)
		}
	}
	return opts
}

func captureSlog(buf *bytes.Buffer) func() {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() {
		slog.SetDefault(old)
	}
}
