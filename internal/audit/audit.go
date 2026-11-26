// Package audit will host an optional local pass-through sidecar. It is a
// stub for now. When implemented it will only ever listen on a loopback
// address, so no other machine can reach it.
package audit

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// ErrNotLoopback is returned for listen addresses that are not loopback.
var ErrNotLoopback = errors.New("audit sidecar may only listen on a loopback address (127.0.0.1, ::1 or localhost)")

// ValidateListen accepts host:port where host is a loopback IP or "localhost".
func ValidateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}

// Serve validates addr and reports that the sidecar is not implemented yet.
func Serve(addr string, w io.Writer) error {
	if err := ValidateListen(addr); err != nil {
		return err
	}
	fmt.Fprintf(w, "audit sidecar on %s: not implemented yet\n", addr)
	return nil
}
