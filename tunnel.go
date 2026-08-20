package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
)

// Tunnel 是一条可复用的探测通道：direct 或某个代理节点。
type Tunnel interface {
	Name() string   // "direct" 或节点名
	Type() string   // "direct" / "ss" / "vmess" ...
	Server() string // 节点 server:port，direct 为空
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// UDP 能力（STUN / DNS-UDP 探测用）：addr 为目标 host:port，
	// 返回的 PacketConn 向该地址收发报文即走隧道。
	SupportsUDP() bool
	ListenPacket(ctx context.Context, addr string) (net.PacketConn, error)
}

type directTunnel struct{}

func (directTunnel) Name() string   { return "direct" }
func (directTunnel) Type() string   { return "direct" }
func (directTunnel) Server() string { return "" }
func (directTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (directTunnel) SupportsUDP() bool { return true }

func (directTunnel) ListenPacket(ctx context.Context, addr string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, "udp", ":0")
}

// Direct 是全局唯一直连通道。
var Direct Tunnel = directTunnel{}

type proxyTunnel struct {
	name   string
	typ    string
	server string
	proxy  C.ProxyAdapter
}

func (p *proxyTunnel) Name() string   { return p.name }
func (p *proxyTunnel) Type() string   { return p.typ }
func (p *proxyTunnel) Server() string { return p.server }

func (p *proxyTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	md := &C.Metadata{
		NetWork: C.TCP,
		Type:    C.INNER,
		Host:    host,
		DstPort: uint16(port),
	}
	return p.proxy.DialContext(ctx, md)
}

func (p *proxyTunnel) SupportsUDP() bool { return p.proxy.SupportUDP() }

func (p *proxyTunnel) ListenPacket(ctx context.Context, addr string) (net.PacketConn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	md := &C.Metadata{
		NetWork: C.UDP,
		Type:    C.INNER,
		Host:    host,
		DstPort: uint16(port),
	}
	return p.proxy.ListenPacketContext(ctx, md)
}

func newProxyTunnel(p C.ProxyAdapter) (Tunnel, error) {
	return &proxyTunnel{
		name:   p.Name(),
		typ:    strings.ToLower(p.Type().String()),
		server: p.Addr(),
		proxy:  p,
	}, nil
}

// BuildTunnel 按 `--via` 参数选择通道：数字按序号（1 起），否则按名称精确匹配。
func BuildTunnel(tunnels []Tunnel, via string) (Tunnel, error) {
	if via == "" || via == "direct" {
		return Direct, nil
	}
	if idx, err := strconv.Atoi(via); err == nil {
		if idx >= 1 && idx <= len(tunnels) {
			return tunnels[idx-1], nil
		}
		return nil, fmt.Errorf("--via 序号 %d 超出范围（共 %d 个节点）", idx, len(tunnels))
	}
	for _, t := range tunnels {
		if t.Name() == via {
			return t, nil
		}
	}
	return nil, fmt.Errorf("找不到节点 %q", via)
}

// ParseNode 将 clash 风格的节点 map 解析为 mihomo 出站。
func ParseNode(m map[string]any) (C.ProxyAdapter, error) {
	p, err := adapter.ParseProxy(m)
	if err != nil {
		return nil, err
	}
	return p, nil
}
