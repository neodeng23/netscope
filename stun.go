package main

// 最小 STUN 客户端（RFC 5389 Binding Request / XOR-MAPPED-ADDRESS），
// 用于经节点的 UDP 能力探测与 UDP ping。

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"time"
)

const (
	stunMagicCookie = 0x2112A442
	stunBindingReq  = 0x0001
	stunBindingOK   = 0x0101
	stunXorMapped   = 0x0020
)

var stunCookieBytes = [4]byte{0x21, 0x12, 0xA4, 0x42}

// stunRequest 构造一个无属性的 Binding Request。
func stunRequest() []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:], stunBindingReq)
	binary.BigEndian.PutUint16(b[2:], 0)
	binary.BigEndian.PutUint32(b[4:], stunMagicCookie)
	rand.Read(b[8:20])
	return b
}

// stunParseResponse 解析 Binding Response，返回 XOR-MAPPED-ADDRESS（host:port）。
func stunParseResponse(b []byte) (string, error) {
	if len(b) < 20 {
		return "", fmt.Errorf("短报文")
	}
	if typ := binary.BigEndian.Uint16(b[0:]); typ != stunBindingOK {
		return "", fmt.Errorf("非成功响应（type=0x%04x）", typ)
	}
	if b[4] != stunCookieBytes[0] || b[5] != stunCookieBytes[1] || b[6] != stunCookieBytes[2] || b[7] != stunCookieBytes[3] {
		return "", fmt.Errorf("magic cookie 不匹配")
	}
	msgLen := int(binary.BigEndian.Uint16(b[2:]))
	if len(b) < 20+msgLen {
		return "", fmt.Errorf("报文截断")
	}
	end := 20 + msgLen
	p := 20
	for p+4 <= end {
		atyp := binary.BigEndian.Uint16(b[p:])
		alen := int(binary.BigEndian.Uint16(b[p+2:]))
		if p+4+alen > end {
			break
		}
		if atyp == stunXorMapped && alen >= 8 && b[p+5] == 0x01 { // IPv4
			port := binary.BigEndian.Uint16(b[p+6:]) ^ uint16(stunMagicCookie>>16)
			var ip [4]byte
			for i := 0; i < 4; i++ {
				ip[i] = b[p+8+i] ^ stunCookieBytes[i]
			}
			return net.JoinHostPort(net.IP(ip[:]).String(), strconv.Itoa(int(port))), nil
		}
		p += 4 + alen
		if alen%4 != 0 { // STUN 属性按 4 字节对齐补 padding
			p += 4 - alen%4
		}
	}
	return "", fmt.Errorf("响应中没有 XOR-MAPPED-ADDRESS")
}

// UDPProbe 经通道向 STUN 服务器发一次 Binding Request，验证 UDP 可用并取回出口地址。
func UDPProbe(ctx context.Context, t Tunnel, server string, timeout time.Duration) (exitAddr string, rttMs float64, err error) {
	pc, err := t.ListenPacket(ctx, server)
	if err != nil {
		return "", 0, err
	}
	defer pc.Close()
	raddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	if _, err := pc.WriteTo(stunRequest(), raddr); err != nil {
		return "", 0, err
	}
	_ = pc.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1500)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return "", 0, err
		}
		rttMs = float64(time.Since(start).Microseconds()) / 1000.0
		if addr, err := stunParseResponse(buf[:n]); err == nil {
			return addr, rttMs, nil
		}
		// 非 STUN 响应（或解析失败）继续读直到超时
	}
}

// UDPPingResult 是一次经通道的 UDP 探测汇总。
type UDPPingResult struct {
	Supported bool       `json:"supported"` // 通道是否声明支持 UDP
	OK        bool       `json:"ok"`        // 是否至少一次往返成功
	ExitAddr  string     `json:"exitAddr,omitempty"`
	Stats     *PingStats `json:"stats,omitempty"`
	Err       string     `json:"err,omitempty"`
}

// STUNPing 经通道做 count 次 STUN 往返测量（UDP ping）。
func STUNPing(ctx context.Context, t Tunnel, server string, count int, timeout, interval time.Duration) UDPPingResult {
	r := UDPPingResult{Supported: t.SupportsUDP()}
	if !r.Supported {
		r.Err = "通道不支持 UDP"
		return r
	}
	st := PingStats{Node: t.Name(), Target: "udp://" + server, Sent: count}
	var rtts []float64
	var lastErr string
loop:
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			st.Sent = i
			break
		}
		addr, rtt, err := UDPProbe(ctx, t, server, timeout)
		if err != nil {
			lastErr = cleanErr(err)
		} else {
			rtts = append(rtts, rtt)
			r.ExitAddr = addr
		}
		if i < count-1 && interval > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				st.Sent = i + 1
				break loop
			}
		}
	}
	st.Recv = len(rtts)
	if st.Recv > st.Sent {
		st.Sent = st.Recv
	}
	summarizePing(&st, rtts)
	r.Stats = &st
	r.OK = st.Recv > 0
	if !r.OK {
		r.Err = lastErr
	}
	return r
}
