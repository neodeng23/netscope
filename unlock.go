package main

// sub unlock：流媒体/AI 服务解锁检测。
// 判定端点与规则参考 RegionRestrictionCheck（https://github.com/lmc999/RegionRestrictionCheck），
// 每次检测都由用户手动触发，一次性完成。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// UnlockStatus 是单项服务的解锁状态。
type UnlockStatus string

const (
	UnlockYes    UnlockStatus = "yes"     // 解锁
	UnlockOnly   UnlockStatus = "limited" // 部分解锁（如 Netflix 仅自制剧）
	UnlockNo     UnlockStatus = "no"      // 未解锁 / 地区封锁
	UnlockFailed UnlockStatus = "failed"  // 网络失败或无法判定
)

// UnlockResult 是单项服务的解锁判定。
type UnlockResult struct {
	Service string       `json:"service"`
	Status  UnlockStatus `json:"status"`
	Region  string       `json:"region,omitempty"`
	Note    string       `json:"note,omitempty"`
}

// NodeUnlock 是一个节点的全部解锁结果。
type NodeUnlock struct {
	Node     string         `json:"node"`
	Type     string         `json:"nodeType,omitempty"`
	ExitIP   string         `json:"exitIp,omitempty"`
	Location string         `json:"location,omitempty"`
	Results  []UnlockResult `json:"results"`
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// unlockClient 构建跟随重定向、走隧道的浏览器型客户端。
func unlockClient(t Tunnel, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:     t.DialContext,
			DialTLSContext:  dialTLSVia(t, nil),
			IdleConnTimeout: 30 * time.Second,
		},
		Timeout: timeout,
	}
}

// unlockFetch GET 一个 URL，返回响应体、状态码与重定向后的最终 URL（4xx/5xx 也读体）。
func unlockFetch(ctx context.Context, cl *http.Client, url string, hdr map[string]string) (body string, status int, finalURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", 0, "", err
	}
	req.Header.Set("User-Agent", browserUA)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer resp.Body.Close()
	// 4MB：判定特征可能在页面深处（如 Gemini 的可用性标记），RRC 读全量，这里给足余量
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), resp.StatusCode, resp.Request.URL.String(), nil
}

// unlockPost POST 一个 URL（表单或 JSON 体），返回响应体与状态码。
func unlockPost(ctx context.Context, cl *http.Client, url, contentType, body string, hdr map[string]string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", browserUA)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode, nil
}

// reFind 用正则取第一个捕获组，无匹配返回空串。
func reFind(pattern, s string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// ---------- Netflix ----------
// 参考逻辑：拉两个授权片源页（LEGO Ninjago / Breaking Bad），页面出现 "Oh no!" 表示该内容不可看。
// 两个都不可看 => 仅自制剧（Originals Only）；任一可看 => 解锁，并从页面状态提取地区。

var netflixRegionRe = regexp.MustCompile(`"id":"([A-Z]{2})","countryName"`)

func checkNetflix(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "Netflix"}
	cl := unlockClient(t, timeout)
	hdr := map[string]string{"Accept-Language": "en-US,en;q=0.9"}
	p1, s1, _, e1 := unlockFetch(ctx, cl, "https://www.netflix.com/title/81280792", hdr)
	p2, s2, _, e2 := unlockFetch(ctx, cl, "https://www.netflix.com/title/70143836", hdr)
	failed := func(err error, status int) string {
		if err != nil {
			return cleanErr(err)
		}
		if status >= 400 {
			return fmt.Sprintf("HTTP %d", status)
		}
		return ""
	}
	f1, f2 := failed(e1, s1), failed(e2, s2)
	if f1 != "" || f2 != "" {
		if s1 == 403 || s2 == 403 {
			r.Status, r.Note = UnlockNo, "IP 被封锁（HTTP 403）"
		} else {
			r.Status, r.Note = UnlockFailed, firstNonEmpty(f1, f2)
		}
		return r
	}
	oh1 := strings.Contains(p1, "Oh no!")
	oh2 := strings.Contains(p2, "Oh no!")
	if oh1 && oh2 {
		r.Status = UnlockOnly
		r.Note = "仅自制剧"
		return r
	}
	r.Status = UnlockYes
	if m := netflixRegionRe.FindStringSubmatch(p1); len(m) > 1 {
		r.Region = m[1]
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------- Disney+ ----------
// 参考逻辑：bamgrid 设备注册 -> token 交换 -> graphql 查询 inSupportedLocation/countryCode。

const disneyBearer = "ZGlzbmV5JmJyb3dzZXImMS4wLjA.Cu56AgSfBTDag5NiRA81oLHkDZfu5L3CKadnefEAY84"

func checkDisneyPlus(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "Disney+"}
	cl := unlockClient(t, timeout)
	auth := map[string]string{"Authorization": "Bearer " + disneyBearer}

	// 1) 设备注册拿 assertion
	body, status, err := unlockPost(ctx, cl, "https://disney.api.edge.bamgrid.com/devices",
		"application/json; charset=UTF-8",
		`{"deviceFamily":"browser","applicationRuntime":"chrome","deviceProfile":"windows","attributes":{}}`, auth)
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	if strings.Contains(body, "403 ERROR") || status == 403 {
		r.Status, r.Note = UnlockNo, "IP 被封锁"
		return r
	}
	assertion := reFind(`"assertion"\s*:\s*"([^"]+)"`, body)
	if assertion == "" {
		r.Status, r.Note = UnlockFailed, fmt.Sprintf("设备注册响应异常（HTTP %d）", status)
		return r
	}

	// 2) assertion 换 refresh_token
	tokenBody := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Atoken-exchange" +
		"&latitude=0&longitude=0&platform=browser" +
		"&subject_token=" + assertion +
		"&subject_token_type=urn%3Abamtech%3Aparams%3Aoauth%3Atoken-type%3Adevice"
	body, status, err = unlockPost(ctx, cl, "https://disney.api.edge.bamgrid.com/token",
		"application/x-www-form-urlencoded", tokenBody, auth)
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	if strings.Contains(body, "forbidden-location") || strings.Contains(body, "403 ERROR") || status == 403 {
		r.Status, r.Note = UnlockNo, "地区不可用"
		return r
	}
	refresh := reFind(`"refresh_token"\s*:\s*"([^"]+)"`, body)
	if refresh == "" {
		r.Status, r.Note = UnlockFailed, "token 响应异常"
		return r
	}

	// 3) graphql 查询地区与支持状态
	gql := `{"query":"mutation refreshToken($input: RefreshTokenInput!) {` +
		`\n            refreshToken(refreshToken: $input) {\n                activeSession {\n                    sessionId\n                }\n            }\n        }` +
		`,"variables":{"input":{"refreshToken":"` + refresh + `"}}}`
	body, status, err = unlockPost(ctx, cl, "https://disney.api.edge.bamgrid.com/graph/v1/device/graphql",
		"application/x-www-form-urlencoded", gql,
		map[string]string{"Authorization": disneyBearer})
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	region := reFind(`"countryCode"\s*:\s*"([^"]+)"`, body)
	supported := reFind(`"inSupportedLocation"\s*:\s*(true|false)`, body)
	if region == "" {
		r.Status, r.Note = UnlockNo, fmt.Sprintf("无地区信息（HTTP %d）", status)
		return r
	}
	// 4) 官网重定向检查（preview/unavailable 页）
	_, _, final, err := unlockFetch(ctx, cl, "https://disneyplus.com", nil)
	if err == nil && (strings.Contains(final, "preview") || strings.Contains(final, "unavailable")) {
		r.Status, r.Note = UnlockNo, "官网重定向到不可用页"
		return r
	}
	r.Region = region
	switch supported {
	case "false":
		r.Status, r.Note = UnlockOnly, "该地区即将开通"
	default:
		r.Status = UnlockYes
	}
	return r
}

// ---------- YouTube Premium ----------

// youtubeCookie 与 RegionRestrictionCheck 原文一致（SOCS 用于绕过 Google 同意墙，值错误会导致误判）。
const youtubeCookie = "YSC=FSCWhKo2Zgw; VISITOR_PRIVACY_METADATA=CgJERRIEEgAgYQ%3D%3D; PREF=f7=4000; __Secure-YEC=CgtRWTBGTFExeV9Iayjele2yBjIKCgJERRIEEgAgYQ%3D%3D; SOCS=CAISOAgDEitib3FfaWRlbnRpdHlmcm9udGVuZHVpc2VydmVyXzIwMjQwNTI2LjAxX3AwGgV6aC1DTiACGgYIgMnpsgY; VISITOR_INFO1_LIVE=Di84mAIbgKY; __Secure-BUCKET=CGQ"

func checkYouTube(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "YouTube Premium"}
	cl := unlockClient(t, timeout)
	body, status, _, err := unlockFetch(ctx, cl, "https://www.youtube.com/premium",
		map[string]string{"Accept-Language": "en-US,en;q=0.9", "Cookie": youtubeCookie})
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	if status >= 400 {
		r.Status, r.Note = UnlockFailed, fmt.Sprintf("HTTP %d", status)
		return r
	}
	if strings.Contains(body, "www.google.cn") {
		r.Status, r.Region, r.Note = UnlockNo, "CN", "重定向到 google.cn"
		return r
	}
	if strings.Contains(body, "Premium is not available in your country") {
		r.Status = UnlockNo
		return r
	}
	if strings.Contains(body, "ad-free") {
		r.Status = UnlockYes
		r.Region = reFind(`"INNERTUBE_CONTEXT_GL"\s*:\s*"([^"]+)"`, body)
		return r
	}
	r.Status, r.Note = UnlockFailed, "页面特征缺失，无法判定"
	return r
}

// ---------- ChatGPT ----------

func checkChatGPT(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "ChatGPT"}
	cl := unlockClient(t, timeout)
	b1, _, _, e1 := unlockFetch(ctx, cl, "https://api.openai.com/compliance/cookie_requirements", nil)
	b2, _, _, e2 := unlockFetch(ctx, cl, "https://ios.chat.openai.com/", nil)
	if e1 != nil && e2 != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(e1)
		return r
	}
	unsupported := e1 == nil && strings.Contains(b1, "unsupported_country")
	vpn := e2 == nil && strings.Contains(b2, "VPN")
	switch {
	case !vpn && !unsupported:
		r.Status = UnlockYes
	case vpn && unsupported:
		r.Status = UnlockNo
	case vpn:
		r.Status, r.Note = UnlockNo, "仅网页可用"
	default:
		r.Status, r.Note = UnlockNo, "仅移动 App 可用"
	}
	return r
}

// ---------- Claude ----------

func checkClaude(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "Claude"}
	cl := unlockClient(t, timeout)
	_, status, final, err := unlockFetch(ctx, cl, "https://claude.ai/", nil)
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	switch {
	case status >= 400:
		r.Status, r.Note = UnlockFailed, fmt.Sprintf("HTTP %d", status)
	case final == "https://claude.ai/" || final == "https://claude.ai":
		r.Status = UnlockYes
	case strings.Contains(final, "app-unavailable-in-region"):
		r.Status = UnlockNo
	default:
		r.Status, r.Note = UnlockFailed, "跳转到 "+final
	}
	return r
}

// ---------- Gemini ----------

func checkGemini(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "Gemini"}
	cl := unlockClient(t, timeout)
	body, status, _, err := unlockFetch(ctx, cl, "https://gemini.google.com", nil)
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	if status >= 400 {
		r.Status, r.Note = UnlockFailed, fmt.Sprintf("HTTP %d", status)
		return r
	}
	if strings.Contains(body, "45631641,null,true") {
		r.Status = UnlockYes
		r.Region = reFind(`,2,1,200,"([A-Z]{3})"`, body)
		return r
	}
	r.Status = UnlockNo
	return r
}

// ---------- Telegram ----------

func checkTelegram(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	r := UnlockResult{Service: "Telegram"}
	cl := unlockClient(t, timeout)
	_, status, _, err := unlockFetch(ctx, cl, "https://web.telegram.org/", nil)
	if err != nil {
		r.Status, r.Note = UnlockFailed, cleanErr(err)
		return r
	}
	if status < 400 {
		r.Status = UnlockYes
	} else {
		r.Status, r.Note = UnlockNo, fmt.Sprintf("HTTP %d", status)
	}
	return r
}

// ---------- TikTok ----------
// 参考逻辑（lmc999/TikTokCheck）：GET 首页，页面里能直接提取 "region":"XX" 即解锁；
// 跟随重定向后才能提取到 => 疑似 IDC 出口（部分解锁）；完全无 region => 失败/受限。

var tiktokRegionRe = regexp.MustCompile(`"region":"([A-Z]{2})"`)

// tiktokHomeURL / spotifySignupURL 可在测试中替换为本地服务器。
var (
	tiktokHomeURL    = "https://www.tiktok.com/"
	spotifySignupURL = "https://spclient.wg.spotify.com/signup/public/v1/account"
)

func checkTikTok(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	cl := unlockClient(t, timeout)
	body, status, finalURL, err := unlockFetch(ctx, cl, tiktokHomeURL, nil)
	if err != nil || status == 0 {
		return UnlockResult{Service: "TikTok", Status: UnlockFailed, Note: shortErr(err.Error())}
	}
	if region := tiktokRegionRe.FindStringSubmatch(body); region != nil {
		if finalURL == tiktokHomeURL || strings.TrimSuffix(finalURL, "/") == strings.TrimSuffix(tiktokHomeURL, "/") {
			return UnlockResult{Service: "TikTok", Status: UnlockYes, Region: region[1]}
		}
		return UnlockResult{Service: "TikTok", Status: UnlockOnly, Region: region[1], Note: "疑似IDC出口"}
	}
	return UnlockResult{Service: "TikTok", Status: UnlockFailed, Note: fmt.Sprintf("首页无区域标记(HTTP %d)，疑似受限", status)}
}

// ---------- Spotify ----------
// 参考逻辑（RegionRestrictionCheck）：POST 注册接口，返回 JSON 里
// status==311 且 is_country_launched==true => 可注册（解锁，含地区）；
// status 320/120 或 is_country_launched==false => 地区未开放。

func checkSpotify(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult {
	cl := unlockClient(t, timeout)
	form := "birth_day=11&birth_month=11&birth_year=2000&collect_personal_info=undefined&creation_flow=&creation_point=https%3A%2F%2Fwww.spotify.com%2Fhk-en%2F&displayname=Gay%20Lord&gender=male&iagree=1&key=a1e486e2729f46d6bb368d6b2bcda326&platform=www&referrer=&send-email=0&thirdpartyemail=0&identifier_token=AgE6YTvEzkReHNfJpO114514"
	body, _, err := unlockPost(ctx, cl, spotifySignupURL,
		"application/x-www-form-urlencoded", form, map[string]string{"Accept-Language": "en"})
	if err != nil {
		return UnlockResult{Service: "Spotify", Status: UnlockFailed, Note: shortErr(err.Error())}
	}
	var v struct {
		Status         int    `json:"status"`
		Country        string `json:"country"`
		IsCountryLaunched *bool `json:"is_country_launched"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Status == 0 {
		return UnlockResult{Service: "Spotify", Status: UnlockFailed, Note: "响应无法解析"}
	}
	switch {
	case v.Status == 311 && v.IsCountryLaunched != nil && *v.IsCountryLaunched:
		return UnlockResult{Service: "Spotify", Status: UnlockYes, Region: v.Country}
	case v.Status == 320 || v.Status == 120:
		return UnlockResult{Service: "Spotify", Status: UnlockNo}
	case v.IsCountryLaunched != nil && !*v.IsCountryLaunched:
		return UnlockResult{Service: "Spotify", Status: UnlockNo}
	}
	return UnlockResult{Service: "Spotify", Status: UnlockFailed, Note: fmt.Sprintf("status=%d", v.Status)}
}

// ---------- 调度 ----------

type unlockChecker struct {
	name string
	fn   func(ctx context.Context, t Tunnel, timeout time.Duration) UnlockResult
}

var unlockCheckers = []unlockChecker{
	{"Netflix", checkNetflix},
	{"Disney+", checkDisneyPlus},
	{"YouTube", checkYouTube},
	{"ChatGPT", checkChatGPT},
	{"Claude", checkClaude},
	{"Gemini", checkGemini},
	{"Telegram", checkTelegram},
	{"TikTok", checkTikTok},
	{"Spotify", checkSpotify},
}

// UnlockNode 对单个节点跑全部选中的服务检测（服务串行，避免触发风控）。
func UnlockNode(ctx context.Context, t Tunnel, timeout time.Duration, services []string) NodeUnlock {
	nu := NodeUnlock{Node: t.Name(), Type: t.Type()}
	if info, err := LookupIP(ctx, t, ""); err == nil {
		nu.ExitIP = info.Query
		nu.Location = info.Location()
	}
	for _, c := range unlockCheckers {
		if len(services) > 0 && !matchAnyFold(services, c.name) {
			continue
		}
		res := c.fn(ctx, t, timeout)
		nu.Results = append(nu.Results, res)
	}
	return nu
}

// matchAnyFold 判断 name 是否匹配 filters 中任意一项（子串、大小写不敏感）。
func matchAnyFold(filters []string, name string) bool {
	for _, f := range filters {
		if f != "" && strings.Contains(strings.ToLower(name), strings.ToLower(f)) {
			return true
		}
	}
	return false
}

// unlockCell 把单项结果格式化为表格单元。
func unlockCell(r UnlockResult) string {
	switch r.Status {
	case UnlockYes:
		if r.Region != "" {
			return "✅ " + r.Region
		}
		return "✅"
	case UnlockOnly:
		if r.Note != "" {
			return "🟡 " + r.Note
		}
		return "🟡"
	case UnlockNo:
		return "❌"
	default:
		if r.Note != "" {
			return "⚠️ " + shortErr(r.Note)
		}
		return "⚠️"
	}
}

// cmdSubUnlock 实现 `sub unlock`。
func cmdSubUnlock(ctx context.Context, args []string) int {
	fs := newFlagSet("sub unlock")
	sf := addSubFlags(fs, 5)
	services := fs.String("services", "", "只检测指定服务（逗号分隔，子串匹配，如 Netflix,ChatGPT）")
	sf.timeout.Duration = 15 * time.Second // 单项服务默认 15s（比普通检测宽松）
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	csvOut := fs.String("csv", "", "CSV 输出路径")
	fs.Parse(args)

	var svcFilter []string
	if s := strings.TrimSpace(*services); s != "" {
		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				svcFilter = append(svcFilter, f)
			}
		}
	} else if len(appConfig.Unlock.Services) > 0 {
		svcFilter = appConfig.Unlock.Services // 配置文件里的默认服务过滤
	}
	// 输出列按选中的服务排
	var cols []unlockChecker
	for _, c := range unlockCheckers {
		if len(svcFilter) > 0 && !matchAnyFold(svcFilter, c.name) {
			continue
		}
		cols = append(cols, c)
	}

	nodes := sf.load(ctx)
	var mu sync.Mutex
	var results []NodeUnlock
	var tasks []func(context.Context)
	for _, n := range nodes {
		n := n
		tasks = append(tasks, func(ctx context.Context) {
			nu := UnlockNode(ctx, n, sf.timeout.Duration, svcFilter)
			mu.Lock()
			results = append(results, nu)
			mu.Unlock()
			yes := 0
			for _, r := range nu.Results {
				if r.Status == UnlockYes {
					yes++
				}
			}
			// 出口 IP 查询失败不代表检测失败（ip-api 不可用时 Results 仍有效）
			exit := nu.ExitIP
			if exit == "" {
				exit = "未知出口"
			}
			Progress("  %s 出口 %s，解锁 %d/%d\n", n.Name(), exit, yes, len(nu.Results))
		})
	}
	RunParallel(ctx, *sf.conc, tasks)

	headers := []string{"节点", "出口", "归属地"}
	for _, c := range cols {
		headers = append(headers, c.name)
	}
	var rows [][]string
	anyYes := false
	for _, nu := range results {
		row := []string{nu.Node, orDash(nu.ExitIP), orDash(nu.Location)}
		bySvc := map[string]UnlockResult{}
		for _, r := range nu.Results {
			bySvc[r.Service] = r
		}
		for _, c := range cols {
			if r, ok := bySvc[c.name]; ok {
				if r.Status == UnlockYes {
					anyYes = true
				}
				row = append(row, unlockCell(r))
			} else {
				row = append(row, "-")
			}
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, map[string]any{"results": results})
	writeCSVIfSet(*csvOut, headers, rows)
	return boolCode(anyYes)
}
