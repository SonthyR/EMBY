//go:build !linux

package main

import "net"

// readTCPConnStats returns unavailable stats on non-Linux hosts where Linux TCP_INFO is not present.
func readTCPConnStats(net.Conn) (retrans int, rttMs int, ok bool) {
	return -1, -1, false
}
