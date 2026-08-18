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

// IPInfo 是一个 IP 的归属地与标记信息（ip-api）。
type IPInfo struct {
	Query      string `json:"query"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	ISP        string `json:"isp"`
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
	return strings.Join(f, "/")
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

// lookupCache 按 tunnel+ip 缓存查询结果。
var lookupCache sync.Map

// LookupIP 查询 IP 归属地（经隧道）。ip 为空时查询本机（该隧道的出口）IP。
func LookupIP(ctx context.Context, t Tunnel, ip string) (*IPInfo, error) {
	key := t.Name() + "|" + ip
	if v, ok := lookupCache.Load(key); ok {
		if info, ok := v.(*IPInfo); ok {
			return info, nil
		}
	}
	u := "http://ip-api.com/json/"
	if ip != "" {
		u += ip
	}
	u += "?lang=zh-CN&fields=status,message,country,regionName,city,isp,proxy,hosting,query"
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
	lookupCache.Store(key, &info)
	return &info, nil
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
