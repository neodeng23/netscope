//go:build linux

package main

import (
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// Linux：UDP socket 开 IP_RECVERR，从 errqueue 读 ICMP 差错（无需 root）。

type sockExtendedErr struct {
	Errno    uint32
	Origin   uint8
	Type     uint8
	Code     uint8
	Pad      uint8
	Info     uint32
	Data     uint32
}

const (
	recvErrBufSize = 1280
	msgErrqueue    = syscall.MSG_ERRQUEUE
)

// probeHopErrqueue 对已写入探测包的 UDP socket 读取 errqueue，返回（发生差错的来源IP，是否目标不可达）。
func probeHopErrqueue(conn *net.UDPConn, timeout time.Duration) (net.IP, bool, error) {
	var hopIP net.IP
	final := false
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := conn.SyscallConn()
		if err != nil {
			return nil, false, err
		}
		var opErr error
		opErr = nil
		raw.Control(func(fd uintptr) {
			buf := make([]byte, recvErrBufSize)
			oob := make([]byte, 1024)
			n, oobn, _, _, e := syscall.Recvmsg(int(fd), buf, oob, msgErrqueue)
			if e != nil {
				if e == syscall.EAGAIN {
					time.Sleep(20 * time.Millisecond)
					return
				}
				opErr = e
				return
			}
			_ = n
			cmsgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				opErr = err
				return
			}
			for _, cm := range cmsgs {
				if cm.Header.Level == syscall.IPPROTO_IP && cm.Header.Type == syscall.IP_RECVERR && len(cm.Data) >= int(unsafe.Sizeof(sockExtendedErr{})) {
					see := (*sockExtendedErr)(unsafe.Pointer(&cm.Data[0]))
					switch see.Type {
					case 11: // time exceeded：来源即该跳路由
						sa := cm.Data[int(unsafe.Sizeof(sockExtendedErr{})):]
						if len(sa) >= 16 && sa[1] == syscall.AF_INET {
							hopIP = net.IPv4(sa[4], sa[5], sa[6], sa[7])
						}
					case 3: // dest unreachable：来源是目标（或中间路由）
						sa := cm.Data[int(unsafe.Sizeof(sockExtendedErr{})):]
						if len(sa) >= 16 && sa[1] == syscall.AF_INET {
							hopIP = net.IPv4(sa[4], sa[5], sa[6], sa[7])
						}
						final = true
					}
				}
			}
		})
		if opErr != nil {
			return nil, false, opErr
		}
		if hopIP != nil {
			return hopIP, final, nil
		}
	}
	return nil, false, fmt.Errorf("超时")
}

// enableRecvErr 打开 IP_RECVERR。
func enableRecvErr(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	raw.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_RECVERR, 1)
	})
	return opErr
}
