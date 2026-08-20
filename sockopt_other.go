//go:build !windows

package main

// setTTL 设置 socket 的 IP TTL（Windows 与 unix 的 syscall 签名不同，拆开实现）。

import "syscall"

func setTTL(fd uintptr, ttl int) error {
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
}
