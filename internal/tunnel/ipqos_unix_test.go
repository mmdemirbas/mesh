//go:build !windows

package tunnel

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDialerControlIPQoS_NegativeReturnsNil(t *testing.T) {
	t.Parallel()
	if got := dialerControlIPQoS(-1); got != nil {
		t.Fatalf("tos=-1 returned non-nil control function, want nil")
	}
}

func TestDialerControlIPQoS_NonNegativeReturnsControl(t *testing.T) {
	t.Parallel()
	for _, tos := range []int{0, 0x10, 0xb8, 0xe0} {
		if got := dialerControlIPQoS(tos); got == nil {
			t.Fatalf("tos=%#x returned nil control function, want non-nil", tos)
		}
	}
}

// TestDialerControlIPQoS_AppliedOnDial exercises the control function against
// a real loopback connection to confirm it sets IP_TOS without error. This
// catches regressions where the syscall layer drifts (e.g. wrong protocol or
// option constants) — the Dial would then fail instead of succeeding silently.
func TestDialerControlIPQoS_AppliedOnDial(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
		close(accepted)
	}()

	ctrl := dialerControlIPQoS(0x10) // lowdelay
	if ctrl == nil {
		t.Fatal("expected non-nil control function for tos=0x10")
	}
	d := &net.Dialer{Control: ctrl, Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with IPQoS control: %v", err)
	}
	_ = conn.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted the dialed connection")
	}
}
