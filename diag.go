package main

import (
	"context"
	"crypto/tls"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ---------- port probe ----------

type PortResult struct {
	Host      string  `json:"host"`
	Port      int     `json:"port"`
	Proto     string  `json:"proto"` // tcp / udp
	State     string  `json:"state"` // open / closed / filtered / open|filtered
	Err       string  `json:"err,omitempty"`
	LatencyMs float64 `json:"latencyMs,omitempty"`
}

// ProbePorts 探测 host 的端口列表。spec 形如 "80"、"443/tcp"、"53/udp"。
// UDP 仅支持 direct 通道（经代理的 UDP 语义不可靠，P0 不做）。
func ProbePorts(ctx context.Context, t Tunnel, host string, specs []string, timeout time.Duration) []PortResult {
	var out []PortResult
	for _, spec := range specs {
		proto := "tcp"
		portStr := spec
		if i := strings.Index(spec, "/"); i >= 0 {
			portStr, proto = spec[:i], strings.ToLower(spec[i+1:])
		}
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 || port > 65535 {
			out = append(out, PortResult{Host: host, Port: 0, Proto: proto, State: "invalid", Err: "端口格式错误: " + spec})
			continue
		}
		switch proto {
		case "tcp":
			out = append(out, probeTCP(ctx, t, host, port, timeout))
		case "udp":
			out = append(out, probeUDP(ctx, t, host, port, timeout))
		default:
			out = append(out, PortResult{Host: host, Port: port, Proto: proto, State: "invalid", Err: "不支持的协议 " + proto})
		}
	}
	return out
}

func probeTCP(ctx context.Context, t Tunnel, host string, port int, timeout time.Duration) PortResult {
	r := PortResult{Host: host, Port: port, Proto: "tcp"}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	conn, err := t.DialContext(cctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	r.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err == nil {
		conn.Close()
		r.State = "open"
		return r
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		r.State = "closed"
	case strings.Contains(s, "i/o timeout"), strings.Contains(s, "timeout"), strings.Contains(s, "timed out"):
		r.State = "filtered"
	default:
		r.State = "filtered"
	}
	r.Err = cleanErr(err)
	return r
}

// probeUDP：DNS 端口发查询可判 open；其余按 ICMP 不可达判 closed，否则 open|filtered。
func probeUDP(ctx context.Context, t Tunnel, host string, port int, timeout time.Duration) PortResult {
	r := PortResult{Host: host, Port: port, Proto: "udp"}
	if t.Name() != "direct" {
		r.State = "unsupported"
		r.Err = "UDP 探测仅支持 direct 通道"
		return r
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 || ips[0].To4() == nil {
		r.State = "filtered"
		r.Err = "域名解析失败或非 IPv4"
		return r
	}
	ip := ips[0].To4()

	icmpC, icmpErr := newICMPConn()
	if icmpErr == nil {
		defer icmpC.Close()
	}
	udpConn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		r.State = "filtered"
		r.Err = err.Error()
		return r
	}
	defer udpConn.Close()
	laddr := udpConn.LocalAddr().(*net.UDPAddr)

	var payload []byte
	if port == 53 {
		payload = buildDNSQuery("example.com", 1)
	} else {
		payload = []byte("netscope-probe")
	}
	if _, err := udpConn.WriteToUDP(payload, &net.UDPAddr{IP: ip, Port: port}); err != nil {
		r.State = "filtered"
		r.Err = err.Error()
		return r
	}
	// 先看是否有应用层回应
	if err := udpConn.SetReadDeadline(time.Now().Add(timeout)); err == nil {
		buf := make([]byte, 1500)
		for {
			n, from, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if from.IP.Equal(ip) && from.Port == port && n > 0 {
				r.State = "open"
				return r
			}
		}
	}
	// 再看 ICMP port-unreachable
	if icmpErr == nil {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			msg, err := icmpC.Recv(time.Until(deadline))
			if err != nil {
				break
			}
			if msg.Type == 3 && msg.InnerUDPPort == uint16(laddr.Port) {
				r.State = "closed"
				return r
			}
		}
	}
	r.State = "open|filtered"
	return r
}

// ---------- http inspect ----------

type InspectResult struct {
	URL         string   `json:"url"`
	Via         string   `json:"via"`
	RemoteAddr  string   `json:"remoteAddr,omitempty"`
	HTTPVersion string   `json:"httpVersion,omitempty"`
	Status      int      `json:"status,omitempty"`
	TLSVersion  string   `json:"tlsVersion,omitempty"`
	CipherSuite string   `json:"cipherSuite,omitempty"`
	ALPN        string   `json:"alpn,omitempty"`
	CertIssuer  string   `json:"certIssuer,omitempty"`
	CertSubject string   `json:"certSubject,omitempty"`
	CertSANs    int      `json:"certSans"`
	ChainDepth  int      `json:"chainDepth"`
	DaysLeft    int      `json:"certDaysLeft"`
	Notes       []string `json:"notes,omitempty"`
	Err         string   `json:"err,omitempty"`
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLSv1.0"
	case tls.VersionTLS11:
		return "TLSv1.1"
	case tls.VersionTLS12:
		return "TLSv1.2"
	case tls.VersionTLS13:
		return "TLSv1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}

// InspectHTTP 对 URL 做证书与协议体检（经指定隧道）。
func InspectHTTP(ctx context.Context, t Tunnel, rawURL string, timeout time.Duration) InspectResult {
	r := InspectResult{URL: rawURL, Via: t.Name()}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.URL = u.String()
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	addr := net.JoinHostPort(host, port)

	// 1) HTTP 状态与版本（ALPN 协商真实 HTTP 版本）
	tr := &http.Transport{
		DialContext:       t.DialContext,
		DialTLSContext:    dialTLSVia(t, &tls.Config{NextProtos: []string{"h2", "http/1.1"}}),
		ForceAttemptHTTP2: true,
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(hctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", subUA)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		r.Err = cleanErr(err)
		tr.CloseIdleConnections()
		if r.Err != "" && strings.Contains(r.Err, "http: server gave HTTP response to HTTPS client") {
			r.Notes = append(r.Notes, "目标是 HTTP（非 TLS），跳过证书体检")
			r.Err = ""
		}
		if r.Err != "" {
			return r
		}
	}
	if resp != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<10))
		resp.Body.Close()
		r.Status = resp.StatusCode
		r.HTTPVersion = resp.Proto
		if resp.TLS != nil {
			r.ALPN = resp.TLS.NegotiatedProtocol
			r.TLSVersion = tlsVersionName(resp.TLS.Version)
			r.CipherSuite = tls.CipherSuiteName(resp.TLS.CipherSuite)
		}
	}
	tr.CloseIdleConnections()

	// 2) 证书体检（单独握手，InsecureSkipVerify 只为读证书链）
	if u.Scheme != "http" {
		cctx, cancel2 := context.WithTimeout(ctx, timeout)
		defer cancel2()
		raw, err := t.DialContext(cctx, "tcp", addr)
		if err == nil {
			tc := tls.Client(raw, &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         host,
				NextProtos:         []string{"h2", "http/1.1"},
			})
			if err := tc.HandshakeContext(cctx); err == nil {
				cs := tc.ConnectionState()
				r.RemoteAddr = raw.RemoteAddr().String()
				if r.TLSVersion == "" {
					r.TLSVersion = tlsVersionName(cs.Version)
					r.CipherSuite = tls.CipherSuiteName(cs.CipherSuite)
					r.ALPN = cs.NegotiatedProtocol
				}
				certs := cs.PeerCertificates
				r.ChainDepth = len(certs)
				if len(certs) > 0 {
					leaf := certs[0]
					r.DaysLeft = int(time.Until(leaf.NotAfter).Hours() / 24)
					r.CertIssuer = dnString(leaf.Issuer)
					r.CertSubject = dnString(leaf.Subject)
					r.CertSANs = len(leaf.DNSNames)
					if r.DaysLeft < 0 {
						r.Notes = append(r.Notes, fmt.Sprintf("证书已过期 %d 天", -r.DaysLeft))
					} else if r.DaysLeft <= 7 {
						r.Notes = append(r.Notes, fmt.Sprintf("证书 %d 天后到期", r.DaysLeft))
					}
				}
				switch cs.Version {
				case tls.VersionTLS10, tls.VersionTLS11:
					r.Notes = append(r.Notes, "使用过时 TLS 版本")
				}
			} else {
				r.Notes = append(r.Notes, "TLS 握手失败: "+cleanErr(err))
			}
			raw.Close()
		} else {
			r.Notes = append(r.Notes, "建连失败: "+cleanErr(err))
		}
	}
	return r
}

func dnString(d pkix.Name) string {
	var parts []string
	if len(d.Organization) > 0 {
		parts = append(parts, "O="+d.Organization[0])
	}
	if d.CommonName != "" {
		parts = append(parts, "CN="+d.CommonName)
	}
	return strings.Join(parts, " ")
}

// ---------- dns audit ----------

type DNSResult struct {
	Resolver string   `json:"resolver"` // 名称或地址
	Type     string   `json:"type"`     // udp / doh / system
	Addrs    []string `json:"addrs,omitempty"`
	TTL      uint32   `json:"ttl,omitempty"`
	RttMs    float64  `json:"rttMs"`
	Err      string   `json:"err,omitempty"`
}

var defaultResolvers = []struct{ label, addr string }{
	{"223.5.5.5（阿里）", "223.5.5.5:53"},
	{"119.29.29.29（腾讯）", "119.29.29.29:53"},
	{"8.8.8.8（Google）", "8.8.8.8:53"},
	{"1.1.1.1（Cloudflare）", "1.1.1.1:53"},
}

var defaultDoH = []struct{ label, url string }{
	{"DoH alidns", "https://dns.alidns.com/resolve?name=%s&type=A"},
	{"DoH cloudflare", "https://cloudflare-dns.com/dns-query?name=%s&type=A"},
}

// DNSAuditResult 是 dns audit 的完整结果：多 resolver 对比 + 污染检测 + 递归出口。
type DNSAuditResult struct {
	Resolvers []DNSResult   `json:"resolvers"`
	Pollution *DNSPollution `json:"pollution,omitempty"`
	EDNS      []EDNSResult  `json:"edns,omitempty"`
}

// DNSPollution 是明文查询与加密查询的对比结果。
type DNSPollution struct {
	Domain   string   `json:"domain"`
	UDPAddrs []string `json:"udpAddrs,omitempty"` // 明文 UDP 查 8.8.8.8 的结果
	DoHAddrs []string `json:"dohAddrs,omitempty"` // DoH 查 dns.google 的结果（参照）
	DoHErr   string   `json:"dohErr,omitempty"`
	Polluted bool     `json:"polluted"`
	Note     string   `json:"note,omitempty"`
}

// EDNSResult 是 resolver 递归出口与 EDNS Client Subnet 探测结果。
type EDNSResult struct {
	Resolver  string   `json:"resolver"`
	EgressIPs []string `json:"egressIps,omitempty"` // 递归 resolver 的出口 IP
	EgressLoc string   `json:"egressLoc,omitempty"` // 出口归属地
	ECS       string   `json:"ecs,omitempty"`       // 暴露给权威的客户端网段
	Err       string   `json:"err,omitempty"`
}

// DNSAudit 对比多个 resolver 对 domain 的 A 解析，并做污染检测与 EDNS 出口探测。
// via 非空时 DoH 查询经该通道；明文 UDP 查询始终走本机直连（那正是要观测的路径）。
func DNSAudit(ctx context.Context, domain string, via Tunnel) DNSAuditResult {
	res := DNSAuditResult{}
	// 系统 resolver
	start := time.Now()
	sysAddrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	rtt := float64(time.Since(start).Microseconds()) / 1000.0
	sys := DNSResult{Resolver: "system", Type: "system", RttMs: rtt}
	if err != nil {
		sys.Err = cleanErr(err)
	} else {
		sys.Addrs = dedupeIPs(sysAddrs)
	}
	res.Resolvers = append(res.Resolvers, sys)

	// 标准 UDP resolver（本机直连视角）
	for _, rs := range defaultResolvers {
		res.Resolvers = append(res.Resolvers, queryDNSUDP(ctx, rs.label, rs.addr, domain, 2*time.Second))
	}

	// DoH（可经 --via）
	for _, d := range defaultDoH {
		res.Resolvers = append(res.Resolvers, queryDoH(ctx, d.label, d.url, domain, via, 5*time.Second))
	}

	res.Pollution = checkDNSPollution(ctx, domain, via)
	res.EDNS = checkEDNS(ctx)
	return res
}

// checkDNSPollution 对比同一 resolver（Google）的明文 UDP 与 DoH 加密结果：
// 完全不一致说明明文查询在途中被注入了假响应（典型 DNS 污染特征）。
func checkDNSPollution(ctx context.Context, domain string, via Tunnel) *DNSPollution {
	p := &DNSPollution{Domain: domain}
	udp := queryDNSUDP(ctx, "8.8.8.8（明文）", "8.8.8.8:53", domain, 3*time.Second)
	p.UDPAddrs = udp.Addrs
	doh := queryDoH(ctx, "dns.google（加密参照）", "https://dns.google/resolve?name=%s&type=A", domain, via, 6*time.Second)
	p.DoHAddrs = doh.Addrs
	if len(doh.Addrs) == 0 {
		p.DoHErr = doh.Err
		p.Note = "加密参照不可用（可加 --via 节点重试），无法判定"
		return p
	}
	if len(udp.Addrs) == 0 {
		p.Note = "明文 UDP 查询失败: " + udp.Err
		return p
	}
	if sameIPSet(udp.Addrs, doh.Addrs) {
		p.Note = "明文与加密查询结果一致，无污染迹象"
		return p
	}
	p.Polluted = disjointIPSet(udp.Addrs, doh.Addrs)
	if p.Polluted {
		p.Note = "明文查询结果与加密查询完全不一致，疑似在途注入（污染）"
	} else {
		p.Note = "结果不完全一致但有交集，可能是地理解析差异"
	}
	return p
}

func sameIPSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

func disjointIPSet(a, b []string) bool {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if set[s] {
			return false
		}
	}
	return true
}

// checkEDNS 查询 o-o.myaddr.l.google.com 的 TXT 记录：
// 第一条是 resolver 递归出口 IP，第二条（如有）是 EDNS Client Subnet。
func checkEDNS(ctx context.Context) []EDNSResult {
	var out []EDNSResult
	for _, rs := range defaultResolvers {
		r := EDNSResult{Resolver: rs.label}
		txts, err := queryTXTUDP(ctx, rs.addr, "o-o.myaddr.l.google.com", 3*time.Second)
		if err != nil {
			r.Err = cleanErr(err)
			out = append(out, r)
			continue
		}
		for _, txt := range txts {
			if strings.Contains(txt, "/") {
				r.ECS = strings.TrimPrefix(strings.TrimSpace(txt), "edns0-client-subnet ")
			} else if net.ParseIP(txt) != nil {
				r.EgressIPs = append(r.EgressIPs, txt)
			}
		}
		if len(r.EgressIPs) > 0 {
			if info, err := LookupIP(ctx, Direct, r.EgressIPs[0]); err == nil {
				r.EgressLoc = info.Location()
			}
		}
		out = append(out, r)
	}
	return out
}

// queryTXTUDP 向 addr 查询 domain 的 TXT 记录（UDP，本机直连）。
func queryTXTUDP(ctx context.Context, addr, domain string, timeout time.Duration) ([]string, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(buildDNSQuery(domain, 16)); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return parseDNSTXT(buf[:n]), nil
}

func queryDNSUDP(ctx context.Context, label, addr, domain string, timeout time.Duration) DNSResult {
	r := DNSResult{Resolver: label, Type: "udp"}
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	conn, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer conn.Close()
	q := buildDNSQuery(domain, 1)
	start := time.Now()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(q); err != nil {
		r.Err = cleanErr(err)
		return r
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	r.RttMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		r.Err = cleanErr(err)
		return r
	}
	addrs, ttl := parseDNSAnswers(buf[:n])
	if len(addrs) == 0 {
		r.Err = "无 A 记录或解析失败"
		return r
	}
	r.Addrs, r.TTL = addrs, ttl
	return r
}

func queryDoH(ctx context.Context, label, urlTmpl, domain string, via Tunnel, timeout time.Duration) DNSResult {
	r := DNSResult{Resolver: label, Type: "doh"}
	u := fmt.Sprintf(urlTmpl, domain)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", u, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/dns-json")
	start := time.Now()
	resp, err := tunnelHTTPClient(via, timeout).Do(req)
	r.RttMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		r.Err = cleanErr(err)
		return r
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Status int `json:"Status"`
		Answer []struct {
			Name string `json:"name"`
			Type int    `json:"type"`
			TTL  uint32 `json:"TTL"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		r.Err = "DoH 响应解析失败: " + cleanErr(err)
		return r
	}
	if body.Status != 0 {
		r.Err = fmt.Sprintf("DNS 状态码 %d（可能被污染或域名不存在）", body.Status)
	}
	var addrs []string
	var ttl uint32
	for _, a := range body.Answer {
		if a.Type == 1 {
			addrs = append(addrs, a.Data)
			if ttl == 0 || a.TTL < ttl {
				ttl = a.TTL
			}
		}
	}
	r.Addrs, r.TTL = addrs, ttl
	if len(addrs) == 0 && r.Err == "" {
		r.Err = "无 A 记录"
	}
	return r
}

func dedupeIPs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- 最小 DNS 报文编解码 ----------

func buildDNSQuery(domain string, qtype uint16) []byte {
	var b []byte
	b = append(b, 0x12, 0x34)             // ID
	b = append(b, 0x01, 0x00)             // flags: RD=1
	b = append(b, 0, 1, 0, 0, 0, 0, 0, 0) // QDCOUNT=1, AN/NS/AR=0
	for _, label := range strings.Split(domain, ".") {
		if label == "" {
			continue
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, qtype)
	b = binary.BigEndian.AppendUint16(b, 1) // IN
	return b
}

// skipDNSQuestion 返回 answer 区起始偏移；qd 为 question 数，失败返回 -1。
func skipDNSQuestion(msg []byte, qd int) int {
	p := 12
	for i := 0; i < int(qd); i++ {
		for {
			if p >= len(msg) {
				return -1
			}
			l := int(msg[p])
			p++
			if l == 0 {
				break
			}
			if l&0xc0 == 0xc0 { // 压缩指针占 2 字节
				p++
				break
			}
			p += l
		}
		p += 4
	}
	return p
}

// skipDNSName 跳过 answer 中的名字字段（可能是压缩指针），返回新偏移，失败返回 -1。
func skipDNSName(msg []byte, p int) int {
	if p < len(msg) && msg[p]&0xc0 == 0xc0 {
		return p + 2
	}
	for {
		if p >= len(msg) {
			return -1
		}
		l := int(msg[p])
		p++
		if l == 0 {
			return p
		}
		p += l
	}
}

// parseDNSAnswers 解析响应中的 A 记录（支持名字压缩指针）。
func parseDNSAnswers(msg []byte) ([]string, uint32) {
	if len(msg) < 12 {
		return nil, 0
	}
	qd := binary.BigEndian.Uint16(msg[4:6])
	an := binary.BigEndian.Uint16(msg[6:8])
	p := skipDNSQuestion(msg, int(qd))
	if p < 0 {
		return nil, 0
	}
	var addrs []string
	var ttl uint32
	for i := 0; i < int(an) && p < len(msg); i++ {
		p = skipDNSName(msg, p)
		if p < 0 || p+10 > len(msg) {
			break
		}
		typ := binary.BigEndian.Uint16(msg[p : p+2])
		t := binary.BigEndian.Uint32(msg[p+4 : p+8])
		rdlen := int(binary.BigEndian.Uint16(msg[p+8 : p+10]))
		p += 10
		if typ == 1 && rdlen == 4 && p+4 <= len(msg) {
			addrs = append(addrs, net.IP(msg[p:p+4]).String())
			if ttl == 0 || (t > 0 && t < ttl) {
				ttl = t
			}
		}
		p += rdlen
	}
	return addrs, ttl
}

// parseDNSTXT 解析响应中的 TXT 记录（rdata 是若干 length-prefixed 字符串）。
func parseDNSTXT(msg []byte) []string {
	if len(msg) < 12 {
		return nil
	}
	qd := binary.BigEndian.Uint16(msg[4:6])
	an := binary.BigEndian.Uint16(msg[6:8])
	p := skipDNSQuestion(msg, int(qd))
	if p < 0 {
		return nil
	}
	var txts []string
	for i := 0; i < int(an) && p < len(msg); i++ {
		p = skipDNSName(msg, p)
		if p < 0 || p+10 > len(msg) {
			break
		}
		typ := binary.BigEndian.Uint16(msg[p : p+2])
		rdlen := int(binary.BigEndian.Uint16(msg[p+8 : p+10]))
		p += 10
		if typ == 16 && p+rdlen <= len(msg) {
			var sb strings.Builder
			q := p
			for q < p+rdlen {
				l := int(msg[q])
				q++
				if q+l > p+rdlen {
					break
				}
				sb.Write(msg[q : q+l])
				q += l
			}
			if sb.Len() > 0 {
				txts = append(txts, sb.String())
			}
		}
		p += rdlen
	}
	return txts
}

// ---------- ip show ----------

type IPShowResult struct {
	Via           string      `json:"via"`
	Domestic      *ipipResult `json:"domestic,omitempty"`
	IPInternalErr string      `json:"domesticErr,omitempty"`
	Global        *IPInfo     `json:"global,omitempty"`
	GlobalErr     string      `json:"globalErr,omitempty"`
}

// IPShow 国内外双视角出口 IP 体检。
func IPShow(ctx context.Context, t Tunnel) IPShowResult {
	r := IPShowResult{Via: t.Name()}
	wg := make(chan struct{}, 2)
	go func() {
		defer func() { wg <- struct{}{} }()
		if v, err := LookupIPIP(ctx, t); err == nil {
			r.Domestic = v
		} else {
			r.IPInternalErr = cleanErr(err)
		}
	}()
	go func() {
		defer func() { wg <- struct{}{} }()
		if v, err := LookupIP(ctx, t, ""); err == nil {
			r.Global = v
		} else {
			r.GlobalErr = cleanErr(err)
		}
	}()
	<-wg
	<-wg
	return r
}
