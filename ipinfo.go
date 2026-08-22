package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// IPInfo 是一个 IP 的归属地与标记信息（ip-api 结构，其他数据源映射到同一形状）。
type IPInfo struct {
	Query      string `json:"query"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	ISP        string `json:"isp"`
	AS         string `json:"as,omitempty"`
	Mobile     bool   `json:"mobile"`
	Proxy      bool   `json:"proxy"`
	Hosting    bool   `json:"hosting"`
	Status     string `json:"status"`
}

func (i *IPInfo) Location() string {
	if i == nil || i.Status != "success" {
		return ""
	}
	parts := []string{}
	for _, p := range []string{i.Country, i.RegionName, i.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

func (i *IPInfo) Flags() string {
	if i == nil {
		return ""
	}
	var f []string
	if i.Proxy {
		f = append(f, "代理")
	}
	if i.Hosting {
		f = append(f, "机房")
	}
	if i.Mobile {
		f = append(f, "移动")
	}
	return strings.Join(f, "/")
}

// RiskScore 返回 0-100 的 IP 风险分：代理(45) + 机房(35) + 移动网络(10)。
// 无标记（家宽/普通宽带）为 0；查询失败返回 -1（未知）。
// 模型为初版经验值，风险越高越容易被流媒体/AI 服务风控。
func (i *IPInfo) RiskScore() int {
	if i == nil || i.Status != "success" {
		return -1
	}
	risk := 0
	if i.Proxy {
		risk += 45
	}
	if i.Hosting {
		risk += 35
	}
	if i.Mobile {
		risk += 10
	}
	if risk > 100 {
		risk = 100
	}
	return risk
}

func (i *IPInfo) Short() string {
	if i == nil {
		return ""
	}
	loc := i.Location()
	if i.Query != "" {
		if loc != "" {
			return i.Query + " (" + loc + ")"
		}
		return i.Query
	}
	return loc
}

// tunnelHTTPClient 构建一个全部流量都走指定隧道的 http.Client。
func tunnelHTTPClient(t Tunnel, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:         t.DialContext,
		DialTLSContext:      dialTLSVia(t, nil),
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: timeout,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ---------- 可插拔的 IP 质量数据源 ----------

// IPQualitySource 是 IP 质量数据源接口：实现它并注册到 ipQualitySources 即可接入。
// 返回的 *IPInfo 是统一形状（各源的字段映射进来）。
type IPQualitySource interface {
	Name() string
	Lookup(ctx context.Context, t Tunnel, ip string) (*IPInfo, error)
}

// ipQualitySources 按序尝试，第一个成功即用。
// ipwho.is 作为 ip-api 的兜底（ip-api 免费版限 45 次/分）：
// 注意 ipwho.is 无 proxy/hosting/mobile 标记，兜底命中时风险分按"未知"处理更保守。
var ipQualitySources = []IPQualitySource{
	ipAPISource{},
	ipwhoSource{baseURL: "https://ipwho.is"},
}

type ipwhoSource struct{ baseURL string }

func (s ipwhoSource) Name() string { return "ipwho.is" }

func (s ipwhoSource) Lookup(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	u := s.baseURL + "/"
	if ip != "" {
		u += ip
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := tunnelHTTPClient(t, 10*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var v struct {
		IP         string `json:"ip"`
		Success    bool   `json:"success"`
		Message    string `json:"message"`
		Country    string `json:"country"`
		Code       string `json:"country_code"`
		Region     string `json:"region"`
		City       string `json:"city"`
		Connection struct {
			ASN int    `json:"asn"`
			Org string `json:"org"`
			ISP string `json:"isp"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if !v.Success {
		return nil, fmt.Errorf("ipwho.is: %s", orDefault(v.Message, "failed"))
	}
	country := v.Country
	if v.Code != "" {
		country = v.Country + "(" + v.Code + ")"
	}
	return &IPInfo{
		Query:      v.IP,
		Country:    country,
		RegionName: v.Region,
		City:       v.City,
		ISP:        orDefault(v.Connection.ISP, v.Connection.Org),
		AS:         fmt.Sprintf("AS%d %s", v.Connection.ASN, v.Connection.Org),
		Status:     "success",
	}, nil
}

type ipAPISource struct{}

func (ipAPISource) Name() string { return "ip-api" }

func (ipAPISource) Lookup(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	u := "http://ip-api.com/json/"
	if ip != "" {
		u += ip
	}
	u += "?lang=zh-CN&fields=status,message,country,regionName,city,isp,as,mobile,proxy,hosting,query"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := tunnelHTTPClient(t, 10*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var info IPInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	if info.Status != "success" {
		return nil, fmt.Errorf("ip-api: %s", info.Status)
	}
	return &info, nil
}

// lookupCache 按 tunnel+ip 缓存查询结果。
// 缓存只服务于"一次批量检测内不重复查询同一节点"（保护 ip-api 免费版 45 次/分的限制），
// TTL 之外必须过期：用户切换代理节点后，同名通道的出口会变。
var lookupCache sync.Map

const lookupCacheTTL = 5 * time.Minute

type lookupEntry struct {
	info *IPInfo
	at   time.Time
}

// doLookupIP 逐个尝试注册的数据源。
func doLookupIP(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	var lastErr error
	for _, src := range ipQualitySources {
		info, err := src.Lookup(ctx, t, ip)
		if err == nil {
			return info, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// LookupIP 经注册的数据源查询 IP 归属地与质量（带 TTL 缓存，批量检测用）。
// ip 为空时查询本机（该隧道的出口）IP。
func LookupIP(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	key := t.Name() + "|" + ip
	if v, ok := lookupCache.Load(key); ok {
		if e, ok := v.(*lookupEntry); ok && time.Since(e.at) < lookupCacheTTL {
			return e.info, nil
		}
		lookupCache.Delete(key)
	}
	info, err := doLookupIP(ctx, t, ip)
	if err != nil {
		return nil, err
	}
	lookupCache.Store(key, &lookupEntry{info: info, at: time.Now()})
	return info, nil
}

// LookupIPFresh 绕过缓存的实时查询（每次手动触发的单次诊断用：本机体检、ip show），
// 结果同时回填缓存。
func LookupIPFresh(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	info, err := doLookupIP(ctx, t, ip)
	if err != nil {
		return nil, err
	}
	lookupCache.Store(t.Name()+"|"+ip, &lookupEntry{info: info, at: time.Now()})
	return info, nil
}

// ipipResult 是国内视角（myip.ipip.net 文本接口）。
type ipipResult struct {
	IP   string
	Desc string
}

var ipipRe = regexp.MustCompile(`当前 IP：([0-9a-fA-F:.]+)\s*来自于：(.+)`)

// LookupIPIP 经隧道查询国内视角的出口 IP 与归属地描述。
func LookupIPIP(ctx context.Context, t Tunnel) (*ipipResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://myip.ipip.net", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "curl/8.4.0")
	resp, err := tunnelHTTPClient(t, 10*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	m := ipipRe.FindStringSubmatch(strings.TrimSpace(string(b)))
	if m == nil {
		return nil, fmt.Errorf("无法解析 ipip.net 响应: %q", string(b))
	}
	return &ipipResult{IP: m[1], Desc: strings.TrimSpace(m[2])}, nil
}

// ExitAddr 经隧道向第三方索取出口地址（不走外部服务时的兜底）。
func ExitAddr(ctx context.Context, t Tunnel) (string, error) {
	conn, err := t.DialContext(ctx, "tcp", "1.1.1.1:80")
	if err != nil {
		return "", err
	}
	conn.Close()
	local := conn.LocalAddr()
	if local == nil {
		return "", fmt.Errorf("无本地地址")
	}
	h, _, err := net.SplitHostPort(local.String())
	if err != nil {
		return "", err
	}
	return h, nil
}
