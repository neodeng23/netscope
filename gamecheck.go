package main

// 游戏平台连接检测：Steam 与 PlayStation，经指定通道（直连或任一节点）。
// Steam：商店/社区连通性 + 货币区（store 页 priceCurrency，随出口 IP 变化，参考 RegionRestrictionCheck）。
// PSN：商店/账户/官网连通性 + 店面区域（store.playstation.com 重定向路径的国家码随出口 IP，
// 语言部分跟随 Accept-Language，故固定发送 en-US 后国家码即为 IP 信号）。

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GameSite 是单个游戏平台站点的连通性。
type GameSite struct {
	Name      string  `json:"name"`
	OK        bool    `json:"ok"`        // 2xx/3xx
	Reachable bool    `json:"reachable"` // 拿到任何 HTTP 响应
	Status    int     `json:"status"`
	TotalMs   float64 `json:"totalMs"`
	Err       string  `json:"err,omitempty"`
	FinalURL  string  `json:"finalUrl,omitempty"`
}

// SteamCheck 是 Steam 的检测结果。
type SteamCheck struct {
	Via       string    `json:"via"`
	Store     GameSite  `json:"store"`
	Community GameSite  `json:"community"`
	Currency  string    `json:"currency,omitempty"` // TWD
	Region    string    `json:"region,omitempty"`   // 台湾
	Err       string    `json:"err,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// PSNCheck 是 PlayStation 的检测结果。
type PSNCheck struct {
	Via        string    `json:"via"`
	Store      GameSite  `json:"store"`
	Account    GameSite  `json:"account"`
	Web        GameSite  `json:"web"`
	Storefront string    `json:"storefront,omitempty"` // en-tw（语言-国家）
	Region     string    `json:"region,omitempty"`     // 台湾
	Err        string    `json:"err,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// 端点可注入（测试替换为本地服务器）。
var (
	steamStoreURL     = "https://store.steampowered.com/app/761830"
	steamCommunityURL = "https://steamcommunity.com/"
	psnStoreURL       = "https://store.playstation.com/"
	psnAccountURL     = "https://account.sonyentertainmentnetwork.com/"
	psnWebURL         = "https://www.playstation.com/"
)

var steamCurrencyRe = regexp.MustCompile(`priceCurrency[^>]*content="([A-Z]{3})"`)
var storefrontCountryRe = regexp.MustCompile(`^/(?:[a-z]{2}(?:-[a-z]{2,4})*)?-?([a-z]{2})(?:/|$)`)

// steamCurrencyRegion 货币码 -> 地区名（Steam 定价区，随出口 IP）。
var steamCurrencyRegion = map[string]string{
	"CNY": "中国", "HKD": "香港", "TWD": "台湾", "JPY": "日本", "KRW": "韩国",
	"USD": "美国", "EUR": "欧元区", "GBP": "英国", "ARS": "阿根廷", "TRY": "土耳其",
	"RUB": "俄罗斯", "UAH": "乌克兰", "KZT": "哈萨克斯坦", "BRL": "巴西", "MXN": "墨西哥",
	"CAD": "加拿大", "AUD": "澳大利亚", "NZD": "新西兰", "SGD": "新加坡", "MYR": "马来西亚",
	"THB": "泰国", "IDR": "印度尼西亚", "PHP": "菲律宾", "VND": "越南", "INR": "印度",
	"ZAR": "南非", "COP": "哥伦比亚", "CLP": "智利", "PEN": "秘鲁", "SAR": "沙特",
	"AED": "阿联酋", "ILS": "以色列", "PLN": "波兰", "CHF": "瑞士", "SEK": "瑞典",
	"NOK": "挪威", "DKK": "丹麦", "CZK": "捷克", "HUF": "匈牙利", "CRC": "哥斯达黎加",
	"UYU": "乌拉圭", "KWD": "科威特", "QAR": "卡塔尔", "EGP": "埃及",
	"NGN": "尼日利亚", "PKR": "巴基斯坦", "BDT": "孟加拉", "LBP": "黎巴嫩", "JOD": "约旦",
}

// psnStorefrontRegion 店面国家码 -> 地区名（Sony 按出口 IP 分发店面）。
var psnStorefrontRegion = map[string]string{
	"us": "美国", "ca": "加拿大", "mx": "墨西哥", "br": "巴西", "ar": "阿根廷",
	"cl": "智利", "co": "哥伦比亚", "pe": "秘鲁", "gb": "英国", "ie": "爱尔兰",
	"de": "德国", "fr": "法国", "it": "意大利", "es": "西班牙", "pt": "葡萄牙",
	"at": "奥地利", "ch": "瑞士", "be": "比利时", "nl": "荷兰", "lu": "卢森堡",
	"dk": "丹麦", "fi": "芬兰", "no": "挪威", "se": "瑞典", "pl": "波兰",
	"tr": "土耳其", "ru": "俄罗斯", "gr": "希腊", "cz": "捷克", "sa": "沙特",
	"ae": "阿联酋", "za": "南非", "in": "印度", "id": "印度尼西亚", "th": "泰国",
	"my": "马来西亚", "sg": "新加坡", "ph": "菲律宾", "vn": "越南", "hk": "香港",
	"tw": "台湾", "mo": "澳门", "jp": "日本", "kr": "韩国", "cn": "中国",
	"au": "澳大利亚", "nz": "新西兰",
}

// gameFetch 经通道 GET，浏览器 UA + 固定 Accept-Language，跟随重定向，返回连通性与响应体。
func gameFetch(ctx context.Context, t Tunnel, name, url string, timeout time.Duration, readBody bool) (GameSite, string) {
	site := GameSite{Name: name}
	cl := &http.Client{
		Transport: &http.Transport{
			DialContext:     t.DialContext,
			DialTLSContext:  dialTLSVia(t, nil),
			IdleConnTimeout: 30 * time.Second,
		},
		Timeout: timeout,
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		site.Err = err.Error()
		return site, ""
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	start := time.Now()
	resp, err := cl.Do(req)
	site.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		site.Err = friendlyErr(err)
		return site, ""
	}
	defer resp.Body.Close()
	site.Reachable = true
	site.Status = resp.StatusCode
	site.OK = resp.StatusCode < 400
	site.FinalURL = resp.Request.URL.String()
	body := ""
	if readBody {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		body = string(b)
	}
	return site, body
}

// CheckSteam 检测 Steam：商店（含货币区）+ 社区，两站并发。
func CheckSteam(ctx context.Context, t Tunnel, timeout time.Duration) SteamCheck {
	r := SteamCheck{Via: t.Name(), CheckedAt: time.Now()}
	var storeBody string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.Store, storeBody = gameFetch(ctx, t, "商店", steamStoreURL, timeout, true)
	}()
	go func() {
		defer wg.Done()
		r.Community, _ = gameFetch(ctx, t, "社区", steamCommunityURL, timeout, false)
	}()
	wg.Wait()
	if m := steamCurrencyRe.FindStringSubmatch(storeBody); m != nil {
		r.Currency = m[1]
		r.Region = steamCurrencyRegion[m[1]]
		if r.Region == "" {
			r.Region = m[1] // 未收录的货币码原样展示
		}
	}
	if !r.Store.Reachable && !r.Community.Reachable {
		r.Err = "商店与社区均不可达"
	}
	return r
}

// CheckPSN 检测 PlayStation：商店（含店面区域）+ 账户 + 官网，三站并发。
func CheckPSN(ctx context.Context, t Tunnel, timeout time.Duration) PSNCheck {
	r := PSNCheck{Via: t.Name(), CheckedAt: time.Now()}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		r.Store, _ = gameFetch(ctx, t, "商店", psnStoreURL, timeout, false)
	}()
	go func() {
		defer wg.Done()
		r.Account, _ = gameFetch(ctx, t, "账户", psnAccountURL, timeout, false)
	}()
	go func() {
		defer wg.Done()
		r.Web, _ = gameFetch(ctx, t, "官网", psnWebURL, timeout, false)
	}()
	wg.Wait()
	if storefront, region, ok := parseStorefront(r.Store.FinalURL); ok {
		r.Storefront = storefront
		r.Region = region
	}
	if !r.Store.Reachable && !r.Account.Reachable && !r.Web.Reachable {
		r.Err = "全部服务不可达"
	}
	return r
}

// parseStorefront 从最终 URL 提取店面（如 en-tw -> "en-tw", 国家 tw）。
func parseStorefront(finalURL string) (storefront, region string, ok bool) {
	// 形如 https://store.playstation.com/en-tw/pages/latest 或 .../zh-hant-tw/...
	u := finalURL
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[i+1:]
	} else {
		return "", "", false
	}
	seg := u
	if i := strings.Index(seg, "/"); i >= 0 {
		seg = seg[:i]
	}
	parts := strings.Split(seg, "-")
	if len(parts) < 2 {
		return "", "", false
	}
	country := parts[len(parts)-1]
	name := psnStorefrontRegion[country]
	if name == "" {
		name = strings.ToUpper(country)
	}
	return seg, name, true
}
