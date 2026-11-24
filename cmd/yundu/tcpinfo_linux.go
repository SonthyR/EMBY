//go:build linux

package main

import (
	"math"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// readTCPConnStats reads total TCP retransmissions and smoothed RTT (ms) from a Linux TCP socket via TCP_INFO.
func readTCPConnStats(conn net.Conn) (retrans int, rttMs int, ok bool) {
	if conn == nil {
		return -1, -1, false
	}
	sc, hasSyscall := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !hasSyscall {
		return -1, -1, false
	}
	rawConn, err := sc.SyscallConn()
	if err != nil {
		return -1, -1, false
	}
	var ctrlErr error
	retrans = -1
	rttMs = -1
	if err := rawConn.Control(func(fd uintptr) {
		if fd > math.MaxInt {
			ctrlErr = unix.EINVAL
			return
		}
		info, getErr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if getErr != nil || info == nil {
			ctrlErr = getErr
			return
		}
		if info.Total_retrans > math.MaxInt32 {
			retrans = math.MaxInt32
		} else {
			retrans = int(info.Total_retrans)
		}
		if info.Rtt > 0 {
			ms := (uint64(info.Rtt) + 500) / 1000
			if ms > uint64(math.MaxInt32) {
				rttMs = math.MaxInt32
			} else {
				rttMs = int(ms)
			}
			if rttMs == 0 {
				rttMs = 1
			}
		} else {
			rttMs = 0
		}
		ok = true
	}); err != nil || ctrlErr != nil || !ok {
		return -1, -1, false
	}
	return retrans, rttMs, true
}
