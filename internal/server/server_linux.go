//go:build linux

package server

import (
	"net"
	"syscall"
)

// SO_REUSEPORT is defined in syscall for Linux kernels >= 3.9.
const soReusePort = 0xF // syscall.SO_REUSEPORT on Linux

// platformListenConfig returns a net.ListenConfig that enables SO_REUSEPORT
// and TCP_NODELAY on Linux for maximum throughput and low latency.
func platformListenConfig() net.ListenConfig {
	return net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var setSockOptErr error
			if err := c.Control(func(fd uintptr) {
				// SO_REUSEPORT (15) allows multiple processes/goroutines to bind the same port.
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); err != nil {
					setSockOptErr = err
					return
				}
				// TCP_NODELAY disables Nagle's algorithm for lower latency.
				//nolint:errcheck
				syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			}); err != nil {
				return err
			}
			return setSockOptErr
		},
	}
}
