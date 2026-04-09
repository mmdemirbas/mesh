//go:build !windows

package tunnel

import (
	"syscall"
	"testing"
)

func TestSSHSignalNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want syscall.Signal
		ok   bool
	}{
		{"ABRT", syscall.SIGABRT, true},
		{"ALRM", syscall.SIGALRM, true},
		{"FPE", syscall.SIGFPE, true},
		{"HUP", syscall.SIGHUP, true},
		{"ILL", syscall.SIGILL, true},
		{"INT", syscall.SIGINT, true},
		{"KILL", syscall.SIGKILL, true},
		{"PIPE", syscall.SIGPIPE, true},
		{"QUIT", syscall.SIGQUIT, true},
		{"SEGV", syscall.SIGSEGV, true},
		{"TERM", syscall.SIGTERM, true},
		{"USR1", syscall.SIGUSR1, true},
		{"USR2", syscall.SIGUSR2, true},
		{"BOGUS", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
		t.Parallel()
			got, ok := sshSignalNumber(tt.name)
			if ok != tt.ok {
				t.Errorf("sshSignalNumber(%q) ok = %v, want %v", tt.name, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("sshSignalNumber(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestSSHSignalRoundTrip(t *testing.T) {
	t.Parallel()
	signals := []syscall.Signal{
		syscall.SIGABRT, syscall.SIGALRM, syscall.SIGFPE,
		syscall.SIGHUP, syscall.SIGILL, syscall.SIGINT,
		syscall.SIGKILL, syscall.SIGPIPE, syscall.SIGQUIT,
		syscall.SIGSEGV, syscall.SIGTERM, syscall.SIGUSR1,
		syscall.SIGUSR2,
	}
	for _, sig := range signals {
		name := signalName(sig)
		got, ok := sshSignalNumber(name)
		if !ok || got != sig {
			t.Errorf("round-trip failed for %v: signalName=%q, sshSignalNumber=%v (ok=%v)", sig, name, got, ok)
		}
	}
}
