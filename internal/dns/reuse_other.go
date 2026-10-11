//go:build !unix

package dns

import "syscall"

func reuseAddr(network, address string, c syscall.RawConn) error { return nil }
