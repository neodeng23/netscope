package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// ---------- ICMP 基础 ----------

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// icmpMessage 是解析后的 ICMP 报文。
type icmpMessage struct {
	Type         uint8
	Code         uint8
	SrcIP        net.IP // 收到报文的来源
	InnerUDPPort uint16 // 若为错误报文，内层 UDP 源端口（用于 traceroute 匹配）
	PayloadID    uint16 // echo 的 id
	Seq          uint16
}

// protoICMP 是 IP 协议号 1（Windows 的 syscall 包没有 IPPROTO_ICMP 常量）。
const protoICMP = 1

// icmpConn 优先 raw（可收 time-exceeded），失败降级 SOCK_DGRAM（macOS 免权限，需 connect 后收发）。
type icmpConn struct {
	pc    net.PacketConn
	conn  net.Conn // 非 nil 时为已 connect 的 dgram socket
	isRaw bool
}

// newICMPConn 创建监听型连接（traceroute 用）：raw 优先，dgram 兜底。
func newICMPConn() (*icmpConn, error) {
	if pc, err := net.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return &icmpConn{pc: pc, isRaw: true}, nil
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, protoICMP)
	if err != nil {
		return nil, fmt.Errorf("无 ICMP 权限（raw 与 dgram 均失败）: %w", err)
	}
	f := os.NewFile(uintptr(fd), "icmp-dgram")
	pc, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	return &icmpConn{pc: pc}, nil
}

// dialICMP 创建点到点连接（echo ping 用）；macOS dgram 必须 connect 到目标。
func dialICMP(dst net.IP) (*icmpConn, error) {
	if pc, err := net.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return &icmpConn{pc: pc, isRaw: true}, nil
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, protoICMP)
	if err != nil {
		return nil, fmt.Errorf("无 ICMP 权限（raw 与 dgram 均失败）: %w", err)
	}
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], dst.To4())
	if err := syscall.Connect(fd, sa); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("connect ICMP socket: %w", err)
	}
	f := os.NewFile(uintptr(fd), "icmp-dgram")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	return &icmpConn{conn: conn}, nil
}

func (c *icmpConn) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return c.pc.Close()
}

// SendEcho 发送 echo request。
func (c *icmpConn) SendEcho(dst net.IP, id, seq uint16) error {
	pkt := make([]byte, 8+8)
	pkt[0], pkt[1] = 8, 0
	binary.BigEndian.PutUint16(pkt[4:], id)
	binary.BigEndian.PutUint16(pkt[6:], seq)
	binary.BigEndian.PutUint64(pkt[8:], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint16(pkt[2:], icmpChecksum(pkt))
	if c.conn != nil {
		_, err := c.conn.Write(pkt)
		return err
	}
	_, err := c.pc.WriteTo(pkt, &net.IPAddr{IP: dst})
	return err
}

// Recv 带超时读一条 ICMP 报文。
// 注意：macOS 的 SOCK_DGRAM ICMP 读到的数据带外层 IP 头（raw 的 ip4:icmp 不带），此处统一剥掉。
func (c *icmpConn) Recv(timeout time.Duration) (*icmpMessage, error) {
	buf := make([]byte, 1500)
	var n int
	var from net.Addr
	var err error
	if c.conn != nil {
		if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		n, err = c.conn.Read(buf)
	} else {
		if err := c.pc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		n, from, err = c.pc.ReadFrom(buf)
	}
	if err != nil {
		return nil, err
	}
	if n >= 20 && buf[0]>>4 == 4 && buf[0]&0x0f >= 5 && buf[0] != 8 {
		// 带 IPv4 头：源地址在 12:16，载荷在 IHL 之后
		ihl := int(buf[0]&0x0f) * 4
		if n > ihl+8 {
			src := make(net.IP, 4)
			copy(src, buf[12:16])
			if from == nil {
				from = &net.IPAddr{IP: src}
			}
			buf = buf[ihl:]
			n -= ihl
		}
	}
	if n < 8 {
		return nil, fmt.Errorf("短报文")
	}
	msg := &icmpMessage{Type: buf[0], Code: buf[1]}
	if from != nil {
		switch a := from.(type) {
		case *net.IPAddr:
			msg.SrcIP = a.IP
		case *net.UDPAddr:
			msg.SrcIP = a.IP
		}
	}
	switch msg.Type {
	case 0, 8: // echo reply / request
		msg.PayloadID = binary.BigEndian.Uint16(buf[4:])
		msg.Seq = binary.BigEndian.Uint16(buf[6:])
	case 3, 11: // dest-unreachable / time-exceeded：内层含原始 IP+UDP 头
		if n >= 8+20+8 {
			inner := buf[8:]
			ihl := int(inner[0]&0x0f) * 4
			if len(inner) >= ihl+4 {
				msg.InnerUDPPort = binary.BigEndian.Uint16(inner[ihl:])
			}
		}
	}
	return msg, nil
}

// ---------- route ping ----------

// RoutePing 对目标做 ICMP ping，无权限时由调用方降级 TCP ping。
func RoutePing(ctx context.Context, host string, count int, timeout, interval time.Duration) ([]pingLine, *PingStats, error) {
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, nil, fmt.Errorf("解析 %s 失败: %v", host, err)
	}
	ip := ips[0].To4()
	if ip == nil {
		return nil, nil, fmt.Errorf("仅支持 IPv4 目标")
	}
	c, err := dialICMP(ip)
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	id := uint16(os.Getpid() & 0xffff)
	var lines []pingLine
	st := &PingStats{Node: "icmp", Target: host + " (" + ip.String() + ")", Sent: count}
	var rtts []float64
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			st.Sent = i
			break
		}
		start := time.Now()
		if err := c.SendEcho(ip, id, uint16(i+1)); err != nil {
			return lines, st, err
		}
		var rtt time.Duration
		var ok bool
		for {
			msg, err := c.Recv(timeout)
			if err != nil {
				break
			}
			if msg.Type == 0 && (msg.SrcIP.Equal(ip) || !c.isRaw) {
				rtt = time.Since(start)
				ok = true
				break
			}
		}
		if ok {
			rtts = append(rtts, float64(rtt.Microseconds())/1000.0)
			lines = append(lines, pingLine{Seq: i + 1, IP: ip.String(), RTTms: float64(rtt.Microseconds()) / 1000.0})
		} else {
			lines = append(lines, pingLine{Seq: i + 1, IP: ip.String(), Timeout: true})
		}
		if i < count-1 && interval > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
			}
		}
	}
	st.Recv = len(rtts)
	summarizePing(st, rtts)
	return lines, st, nil
}

type pingLine struct {
	Seq     int     `json:"seq"`
	IP      string  `json:"ip"`
	RTTms   float64 `json:"rttMs"`
	Timeout bool    `json:"timeout"`
}

func summarizePing(st *PingStats, rtts []float64) {
	if len(rtts) == 0 {
		st.Loss = 100
		return
	}
	st.Loss = float64(st.Sent-st.Recv) / float64(st.Sent) * 100
	min, max := rtts[0], rtts[0]
	sum := 0.0
	for _, r := range rtts {
		if r < min {
			min = r
		}
		if r > max {
			max = r
		}
		sum += r
	}
	st.MinMs, st.MaxMs, st.AvgMs = min, max, sum/float64(len(rtts))
	if len(rtts) > 1 {
		j := 0.0
		for i := 1; i < len(rtts); i++ {
			d := rtts[i] - rtts[i-1]
			if d < 0 {
				d = -d
			}
			j += d
		}
		st.Jitter = j / float64(len(rtts)-1)
	}
}

// ---------- route trace（UDP + ICMP，无需 root 优先） ----------

type hopLine struct {
	TTL   int     `json:"ttl"`
	IP    string  `json:"ip,omitempty"`
	Loc   string  `json:"location,omitempty"` // 归属地
	RTTms float64 `json:"rttMs,omitempty"`
	Star  bool    `json:"star,omitempty"`
	Final bool    `json:"final,omitempty"` // 到达目标
}

// RouteTrace 递增 TTL 的 UDP 探测。Linux 用 IP_RECVERR（免 root）；
// 其他平台 raw ICMP 监听（macOS 需 root）；两者皆不可用时降级为 TCP TTL 扫描（只有 RTT 无中间跳 IP）。
func RouteTrace(ctx context.Context, host string, maxTTL, probes int, timeout time.Duration) ([]hopLine, error) {
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("解析 %s 失败: %v", host, err)
	}
	ip := ips[0].To4()
	if ip == nil {
		return nil, fmt.Errorf("仅支持 IPv4 目标")
	}

	var hops []hopLine
	if enableRecvErrAvailable() {
		hops, err = routeTraceErrqueue(ctx, ip, maxTTL, probes, timeout)
		if err == nil {
			return hops, nil
		}
	}

	c, err := newICMPConn()
	if err != nil {
		// 最后的降级：TCP TTL 扫描
		return routeTraceSweep(ctx, ip, maxTTL, timeout)
	}
	defer c.Close()
	if !c.isRaw {
		// macOS 非 root 的 dgram socket 收不到 ICMP 差错报文
		c.Close()
		return routeTraceSweep(ctx, ip, maxTTL, timeout)
	}

	events := make(chan *icmpMessage, 64)
	go func() {
		for {
			msg, err := c.Recv(0)
			if err != nil {
				return
			}
			if msg.Type == 11 || msg.Type == 3 {
				select {
				case events <- msg:
				default:
				}
			}
		}
	}()

	reached := false
	for ttl := 1; ttl <= maxTTL && !reached; ttl++ {
		hop := hopLine{TTL: ttl}
		anyReply := false
		var rtt float64
		var hopIP string
		for p := 0; p < probes; p++ {
			if ctx.Err() != nil {
				return hops, ctx.Err()
			}
			udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
			if err != nil {
				return hops, err
			}
			laddr := udpConn.LocalAddr().(*net.UDPAddr)
			dstPort := 33434 + ttl
			p4 := ipv4.NewConn(udpConn)
			if err := p4.SetTTL(ttl); err != nil {
				udpConn.Close()
				return hops, fmt.Errorf("设置 TTL 失败: %w", err)
			}
			start := time.Now()
			if _, err := udpConn.WriteToUDP(makeProbePayload(ttl, p), &net.UDPAddr{IP: ip, Port: dstPort}); err != nil {
				udpConn.Close()
				return hops, err
			}
			match := false
			deadline := time.After(timeout)
			for !match {
				select {
				case msg := <-events:
					if msg.InnerUDPPort == uint16(laddr.Port) {
						match = true
						anyReply = true
						rtt = float64(time.Since(start).Microseconds()) / 1000.0
						hopIP = msg.SrcIP.String()
						if msg.Type == 3 && msg.SrcIP.Equal(ip) {
							reached = true
							hop.Final = true
						}
					}
				case <-deadline:
					match = true
				case <-ctx.Done():
					udpConn.Close()
					return hops, ctx.Err()
				}
			}
			udpConn.Close()
		}
		if anyReply {
			hop.IP = hopIP
			hop.RTTms = rtt
		} else {
			hop.Star = true
		}
		hops = append(hops, hop)
		if info, err := LookupIP(ctx, Direct, hopIP); err == nil && info != nil {
			hop.Loc = info.Location()
		}
	}
	return hops, nil
}

func enableRecvErrAvailable() bool { return runtime.GOOS == "linux" }

// routeTraceErrqueue：Linux 免 root 路径，从 UDP errqueue 读 ICMP 差错。
func routeTraceErrqueue(ctx context.Context, ip net.IP, maxTTL, probes int, timeout time.Duration) ([]hopLine, error) {
	var hops []hopLine
	reached := false
	for ttl := 1; ttl <= maxTTL && !reached; ttl++ {
		hop := hopLine{TTL: ttl}
		anyReply := false
		var rtt float64
		var hopIP string
		for p := 0; p < probes; p++ {
			if ctx.Err() != nil {
				return hops, ctx.Err()
			}
			udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
			if err != nil {
				return hops, err
			}
			if err := enableRecvErr(udpConn); err != nil {
				udpConn.Close()
				return hops, err
			}
			dstPort := 33434 + ttl
			p4 := ipv4.NewConn(udpConn)
			if err := p4.SetTTL(ttl); err != nil {
				udpConn.Close()
				return hops, err
			}
			start := time.Now()
			if _, err := udpConn.WriteToUDP(makeProbePayload(ttl, p), &net.UDPAddr{IP: ip, Port: dstPort}); err != nil {
				udpConn.Close()
				return hops, err
			}
			hopAddr, final, err := probeHopErrqueue(udpConn, timeout)
			elapsed := time.Since(start)
			udpConn.Close()
			if err == nil && hopAddr != nil {
				anyReply = true
				rtt = float64(elapsed.Microseconds()) / 1000.0
				hopIP = hopAddr.String()
				if final && hopAddr.Equal(ip) {
					reached = true
					hop.Final = true
				}
			}
		}
		if anyReply {
			hop.IP = hopIP
			hop.RTTms = rtt
		} else {
			hop.Star = true
		}
		hops = append(hops, hop)
		if info, err := LookupIP(ctx, Direct, hopIP); err == nil && info != nil && hopIP != "" {
			hop.Loc = info.Location()
		}
	}
	return hops, nil
}

// routeTraceSweep：无 ICMP 能力时的降级--TCP 递增 TTL 扫描（connect 前设置 TTL）。
// 只能判断「从第几跳起目标可达」与该跳 RTT，中间路由 IP 内核不透出。
func routeTraceSweep(ctx context.Context, ip net.IP, maxTTL int, timeout time.Duration) ([]hopLine, error) {
	var hops []hopLine
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if ctx.Err() != nil {
			return hops, ctx.Err()
		}
		wantTTL := ttl
		d := &net.Dialer{
			Timeout: timeout,
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					setTTL(fd, wantTTL)
				})
			},
		}
		start := time.Now()
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
		hop := hopLine{TTL: ttl, Star: true}
		if err == nil {
			hop.Star = false
			hop.Final = true
			hop.IP = ip.String()
			hop.RTTms = float64(time.Since(start).Microseconds()) / 1000.0
			conn.Close()
			hops = append(hops, hop)
			return hops, nil
		}
		hops = append(hops, hop)
	}
	hops = append(hops, hopLine{
		IP:   "受限模式：本环境无 root 收不到 ICMP 差错报文，中间跳不可见（macOS 可 sudo 获得完整逐跳；Linux 免 root）",
		Star: true,
	})
	return hops, nil
}
func makeProbePayload(ttl, probe int) []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(ttl*16 + probe + i)
	}
	return b
}
