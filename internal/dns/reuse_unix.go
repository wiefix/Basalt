//go:build unix

package dns

import (
	"syscall"
)

// reuseAddr sets SO_REUSEADDR so the server can bind the wildcard address
// next to a local stub resolver on 127.0.0.53:53 (systemd-resolved).
func reuseAddr(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
