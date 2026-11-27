package manager

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestServerRefused(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	if !serverRefused(fmt.Errorf("connection retry exhausted: %w", refused)) {
		t.Fatal("wrapped refusal not seen")
	}
	if serverRefused(errors.New("read: connection reset by peer")) || serverRefused(nil) {
		t.Fatal("a reset is not a refusal")
	}
}
