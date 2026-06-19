package customerror

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// Error classification for retry logic (inspired by rclone's fserrors)
// Distinguishes transient/retriable errors from permanent failures

// retriableErrorStrings contains error message substrings that indicate retriable errors.
// These catch errors from standard library that aren't exported as typed errors.
var retriableErrorStrings = []string{
	"use of closed network connection",
	"unexpected EOF",
	"connection reset by peer",
	"connection refused",
	"broken pipe",
	"i/o timeout",
	"TLS handshake timeout",
	"no such host",
	"server misbehaving",
	"connection timed out",
	"network is unreachable",
	"no route to host",
	"transport connection broken",
	"http2: client connection lost",
	"http2: server sent GOAWAY",
	"http2: timeout awaiting",
	"stream error:",
	"bad record MAC",
	"server closed idle connection",
	"client connection force closed",
	"context deadline exceeded",
}

// permanentErrorStrings contains error message substrings that indicate non-retriable errors
var permanentErrorStrings = []string{
	"404",
	"not found",
	"403",
	"forbidden",
	"401",
	"unauthorized",
	"402",
	"payment required",
	"410",
	"gone",
	"invalid api key",
	"file not exist",
	"no such file",
}

// IsRetriableError returns true if the error is likely transient and should be retried.
// This follows rclone's multi-layer error classification strategy.
func IsRetriableError(err error) bool {
	if err == nil {
		return false
	}

	var csError *Error
	if errors.As(err, &csError) {
		if csError.IsPermanent() {
			return false
		}
		// If not permanent, consider retriable
		if csError.IsRetryable() {
			return true
		}
		return false
	}

	// Ask the error itself whether it is retriable. This covers types like
	// *nntp.Error that carry their own retryability knowledge but are not
	// *customerror.Error. We intentionally place this after the *Error check
	// so the explicit permanent/retry flags above always win for our own type.
	type selfRetryable interface {
		IsRetryable() bool
	}
	if r, ok := err.(selfRetryable); ok {
		return r.IsRetryable()
	}

	// Check for explicit non-retriable markers first
	if IsPermanentError(err) {
		return false
	}

	// Context deadline exceeded is retriable (timeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Context canceled is NOT retriable (user initiated)
	if errors.Is(err, context.Canceled) {
		return false
	}

	// io.EOF at expected position is not an error
	// io.ErrUnexpectedEOF during transfer is retriable
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// io.ErrClosedPipe means the underlying pipe/connection was closed mid-transfer.
	// This is transient (the remote end reset) and should be retried like EPIPE.
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}

	// Check for net.Error interface (Timeout() and Temporary())
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Timeout errors are always retriable
		if netErr.Timeout() {
			return true
		}
	}

	// Check for specific syscall errors
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}

	// Check error string for known patterns
	errStr := strings.ToLower(err.Error())
	for _, pattern := range retriableErrorStrings {
		if strings.Contains(errStr, strings.ToLower(pattern)) {
			return true
		}
	}

	// Check wrapped errors
	unwrapped := errors.Unwrap(err)
	if unwrapped != nil && !errors.Is(unwrapped, err) {
		return IsRetriableError(unwrapped)
	}

	return false
}

// IsPermanentError returns true if the error should NOT be retried.
// These are typically 4xx HTTP errors or explicit access denials.
func IsPermanentError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	for _, pattern := range permanentErrorStrings {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}
