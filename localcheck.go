package main

// 本机网络体检（与订阅无关，全部直连）：国内/国外双视角。
// 参考 ip111.cn 的双视角思路，额外增加：基准延迟与丢包、站点可达性、
// DNS 污染检测（明文 53 vs DoH）、出口一致性（是否存在分流/透明代理）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SiteCheck 是一个参考站点的可达性。
type SiteCheck struct {
	Name    string  `json:"name"`
	URL     string  `json:"url"`
	OK      bool    `json:"ok"`
	Status  int     `json:"status"`
	TotalMs float64 `json:"totalMs"`
	Err     string  `json:"err,omitempty"`
}

// Perspective 是一个视角（国内/国外）的体检结果。
type Perspective struct {
	Label    string     `json:"label"`
	IP       string     `json:"ip,omitempty"`
	Location string     `json:"location,omitempty"`
	ISP      string     `json:"isp,omitempty"`
	Flags    string     `json:"flags,omitempty"` // 代理/机房/移动
	Risk     int        `json:"risk"`            // 0-100，-1 未知
	Err      string     `json:"err,omitempty"`
	Ping     *PingStats `json:"ping,omitempty"`
	Sites    []SiteCheck `json:"sites"`
}

// LocalCheckResult 是一次本机网络体检的完整结果。
type LocalCheckResult struct {
	Time     time.Time   `json:"time"`
	Domestic *Perspective `json:"domestic"`
	Foreign  *Perspective `json:"foreign"`
	SameExit *bool        `json:"sameExit,omitempty"` // 两个视角出口是否一致
	DNS      struct {
		Polluted *bool  `json:"polluted,omitempty"` // 明文 53 与 DoH 解析不一致 => 疑似污染
		Detail   string `json:"detail,omitempty"`
	} `json:"dns"`
}

// 探测目标可注入（测试替换为本地服务器）。
var (
	localCNSites = []struct{ Name, URL string }{
		{"百度", "https://www.baidu.com"},
		{"腾讯", "https://www.qq.com"},
		{"哔哩哔哩", "https://www.bilibili.com"},
	}
	localGlobalSites = []struct{ Name, URL string }{
		{"Google 204", "https://www.gstatic.com/generate_204"},
		{"GitHub", "https://github.com"},
		{"Cloudflare", "https://www.cloudflare.com"},
	}
	localCNPingTarget    = "223.5.5.5:443"
	localGlobalPingTarget = "1.1.1.1:443"
	dnsPolluteDomain     = "www.google.com"
)

// RunLocalCheck 跑一次本机网络体检（约 5~15 秒，各项目并发）。
func RunLocalCheck(ctx context.Context) LocalCheckResult {
	res := LocalCheckResult{Time: time.Now(),
		Domestic: &Perspective{Label: "domestic"},
		Foreign:  &Perspective{Label: "foreign"}}
	var wg sync.WaitGroup

	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() { // 国内视角：ipip.net 出口 + 基准 ping + 站点
		d := res.Domestic
		if v, err := LookupIPIP(ctx, Direct); err == nil {
			d.IP, d.Location = v.IP, v.Desc
		} else {
			d.Err = cleanErr(err)
		}
		p := TCPPing(ctx, Direct, localCNPingTarget, 4, 3*time.Second, 300*time.Millisecond)
		d.Ping = &p
		d.Sites = checkSites(ctx, localCNSites)
	})

	run(func() { // 国外视角：ip-api 出口 + 基准 ping + 站点
		f := res.Foreign
		if info, err := LookupIP(ctx, Direct, ""); err == nil {
			f.IP = info.Query
			f.Location = info.Location()
			f.ISP = info.ISP
			f.Flags = info.Flags()
			f.Risk = info.RiskScore()
		} else {
			f.Err = cleanErr(err)
		}
		p := TCPPing(ctx, Direct, localGlobalPingTarget, 4, 3*time.Second, 300*time.Millisecond)
		f.Ping = &p
		f.Sites = checkSites(ctx, localGlobalSites)
	})

	run(func() { // DNS 污染：明文 8.8.8.8 vs Cloudflare DoH
		polluted, detail := dnsPollutionCheck(ctx)
		res.DNS.Polluted, res.DNS.Detail = polluted, detail
	})

	wg.Wait()

	if res.Domestic.IP != "" && res.Foreign.IP != "" {
		same := res.Domestic.IP == res.Foreign.IP
		res.SameExit = &same
	}
	return res
}

func checkSites(ctx context.Context, list []struct{ Name, URL string }) []SiteCheck {
	var out []SiteCheck
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range list {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			r := HTTPCheck(cctx, Direct, s.URL, 8*time.Second)
			cancel()
			sc := SiteCheck{Name: s.Name, URL: s.URL, OK: r.OK, Status: r.Status, TotalMs: r.TotalMs, Err: r.Err}
			mu.Lock()
			out = append(out, sc)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// dnsPollutionCheck 对比同一域名在明文 UDP 与 DoH 加密通道的解析结果。
// 两者都有结果且不一致 => 疑似在途注入/污染；任一失败 => 无法判定。
func dnsPollutionCheck(ctx context.Context) (*bool, string) {
	udp := queryDNSUDP(ctx, "8.8.8.8", "8.8.8.8:53", dnsPolluteDomain, 3*time.Second)
	doh := queryDoH(ctx, "DoH cloudflare", "https://cloudflare-dns.com/dns-query?name=%s&type=A", dnsPolluteDomain, Direct, 6*time.Second)
	if udp.Err != "" {
		return nil, fmt.Sprintf("明文 DNS(8.8.8.8:53) 不可达：%s", udp.Err)
	}
	if doh.Err != "" {
		return nil, fmt.Sprintf("DoH 查询失败：%s", doh.Err)
	}
	same := ipSetEqual(udp.Addrs, doh.Addrs)
	polluted := !same
	detail := fmt.Sprintf("明文 %s vs DoH %s", strings.Join(udp.Addrs, " "), strings.Join(doh.Addrs, " "))
	if same {
		return &polluted, detail + "（一致）"
	}
	return &polluted, detail + "（不一致，疑似污染/劫持）"
}

func ipSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]int{}
	for _, x := range a {
		set[x]++
	}
	for _, x := range b {
		set[x]--
		if set[x] < 0 {
			return false
		}
	}
	return true
}
