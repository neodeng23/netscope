//go:build !linux

package main

import (
	"net"
	"time"
)

// 非 Linux 平台：UDP errqueue 不可用，ICMP 差错只能靠 raw socket（macOS 需 root）。
func probeHopErrqueue(conn *net.UDPConn, timeout time.Duration) (net.IP, bool, error) {
	return nil, false, errUnsupported
}

func enableRecvErr(conn *net.UDPConn) error { return errUnsupported }

var errUnsupported = errUnsupportedImpl{}

type errUnsupportedImpl struct{}

func (errUnsupportedImpl) Error() string { return "此平台不支持" }
