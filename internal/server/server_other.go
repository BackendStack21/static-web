//go:build !linux

package server

import "net"

// platformListenConfig returns a default net.ListenConfig for non-Linux platforms.
// SO_REUSEPORT is Linux-specific; on other platforms we use the standard config.
func platformListenConfig() net.ListenConfig {
	return net.ListenConfig{}
}
