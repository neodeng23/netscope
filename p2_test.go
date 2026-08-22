package main

// P2 功能测试：配置预设、快照清理、ipwho.is 数据源、TikTok/Spotify 解锁判定。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 配置 ----------

func TestConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
targets:
  - https://a.example/204
  - https://b.example/204
unlock:
  services:
    - Netflix
    - TikTok
score:
  availability: 30
  latency: 30
  speed: 20
  ipq: 20
serve:
  listen: 127.0.0.1:9999
  token: secret
reports:
  keep: 3
  keepDays: 30
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 2 || cfg.Targets[0] != "https://a.example/204" {
		t.Fatalf("targets: %v", cfg.Targets)
	}
	if len(cfg.Unlock.Services) != 2 || cfg.Unlock.Services[1] != "TikTok" {
		t.Fatalf("unlock services: %v", cfg.Unlock.Services)
	}
	if !cfg.Score.Valid() || cfg.Score.Availability != 30 || cfg.Score.Speed != 20 {
		t.Fatalf("score: %+v", cfg.Score)
	}
	if cfg.Serve.Listen != "127.0.0.1:9999" || cfg.Serve.Token != "secret" {
		t.Fatalf("serve: %+v", cfg.Serve)
	}
	if cfg.Reports.Keep != 3 || cfg.Reports.KeepDays != 30 {
		t.Fatalf("reports: %+v", cfg.Reports)
	}
}

func TestConfigLoadPartialInvalidScore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("score:\n  availability: 10\n"), 0o644)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Score.Valid() {
		t.Fatalf("缺项的权重应判无效: %+v", cfg.Score)
	}
}

func TestConfigLoadMissingFile(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || cfg == nil {
		t.Fatalf("缺失文件应返回零值配置: %v %v", cfg, err)
	}
}

// ---------- 快照清理 ----------

func TestCleanReportsByKeep(t *testing.T) {
	dir := t.TempDir()
	// 5 份快照，每份 html+json 成对
	stamps := []string{"20260101-000000", "20260102-000000", "20260103-000000", "20260104-000000", "20260105-000000"}
	for _, s := range stamps {
		for _, ext := range []string{".html", ".json"} {
			os.WriteFile(filepath.Join(dir, "rate-"+s+ext), []byte("x"), 0o644)
		}
	}
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o644) // 不相关文件不动

	removed, err := cleanReports(dir, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 { // 最旧 2 份 × 2 文件
		t.Fatalf("removed: %v", removed)
	}
	for _, s := range stamps[:2] {
		for _, ext := range []string{".html", ".json"} {
			if _, err := os.Stat(filepath.Join(dir, "rate-"+s+ext)); !os.IsNotExist(err) {
				t.Fatalf("最旧快照 %s%s 未删除", s, ext)
			}
		}
	}
	for _, s := range stamps[2:] {
		if _, err := os.Stat(filepath.Join(dir, "rate-"+s+".html")); err != nil {
			t.Fatalf("新快照 %s 不应被删: %v", s, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "other.txt")); err != nil {
		t.Fatal("不相关文件被误删")
	}
}

func TestCleanReportsByDays(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * 24 * time.Hour).Format("20060102-150405")
	fresh := time.Now().Format("20060102-150405")
	for _, s := range []string{old, fresh} {
		os.WriteFile(filepath.Join(dir, "rate-"+s+".html"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(dir, "rate-"+s+".json"), []byte("x"), 0o644)
	}
	removed, err := cleanReports(dir, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed: %v", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "rate-"+fresh+".html")); err != nil {
		t.Fatal("新快照被误删")
	}
}

// ---------- ipwho.is 数据源 ----------

func TestIPWhoSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/1.2.3.4") {
			http.Error(w, "bad path", 400)
			return
		}
		fmt.Fprint(w, `{"ip":"1.2.3.4","success":true,"country":"United States","country_code":"US","region":"California","city":"Los Angeles","connection":{"asn":15169,"org":"Google LLC","isp":"Google LLC"}}`)
	}))
	defer srv.Close()
	src := ipwhoSource{baseURL: srv.URL}
	info, err := src.Lookup(context.Background(), Direct, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if info.Query != "1.2.3.4" || info.Country != "United States(US)" || info.City != "Los Angeles" {
		t.Fatalf("info: %+v", info)
	}
	if info.ISP != "Google LLC" || !strings.HasPrefix(info.AS, "AS15169") {
		t.Fatalf("connection: %s / %s", info.ISP, info.AS)
	}
	if info.Status != "success" {
		t.Fatalf("status: %s", info.Status)
	}
}

func TestIPWhoSourceFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"message":"Invalid IP address"}`)
	}))
	defer srv.Close()
	if _, err := (ipwhoSource{baseURL: srv.URL}).Lookup(context.Background(), Direct, "bogus"); err == nil {
		t.Fatal("失败响应应返回错误")
	}
}

// ---------- TikTok / Spotify 解锁判定（本地端到端） ----------

func TestCheckTikTok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>... "__UNIVERSAL_DATA_FOR_REHYDRATION__":{"region":"JP"} ...</html>`)
	}))
	defer srv.Close()
	old := tiktokHomeURL
	tiktokHomeURL = srv.URL + "/"
	defer func() { tiktokHomeURL = old }()

	r := checkTikTok(context.Background(), Direct, 3*time.Second)
	if r.Status != UnlockYes || r.Region != "JP" {
		t.Fatalf("应判解锁 JP: %+v", r)
	}
}

func TestCheckTikTokNoRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>login wall</html>`)
	}))
	defer srv.Close()
	old := tiktokHomeURL
	tiktokHomeURL = srv.URL + "/"
	defer func() { tiktokHomeURL = old }()

	r := checkTikTok(context.Background(), Direct, 3*time.Second)
	if r.Status != UnlockFailed {
		t.Fatalf("无区域标记应判失败: %+v", r)
	}
}

func TestCheckSpotify(t *testing.T) {
	cases := []struct {
		resp     string
		want     UnlockStatus
		wantArea string
	}{
		{`{"status":311,"country":"HK","is_country_launched":true}`, UnlockYes, "HK"},
		{`{"status":320,"country":"XX","is_country_launched":true}`, UnlockNo, ""},
		{`{"status":311,"country":"CN","is_country_launched":false}`, UnlockNo, ""},
	}
	// 逐 case 用独立服务器验证
	for i, c := range cases {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, c.resp)
		}))
		old := spotifySignupURL
		spotifySignupURL = s.URL
		got := checkSpotify(context.Background(), Direct, 3*time.Second)
		spotifySignupURL = old
		s.Close()
		if got.Status != c.want || got.Region != c.wantArea {
			t.Fatalf("case %d: %+v (want %s/%s)", i, got, c.want, c.wantArea)
		}
	}
}

func TestUnlockRegistryHasNewServices(t *testing.T) {
	found := map[string]bool{}
	for _, c := range unlockCheckers {
		found[c.name] = true
	}
	for _, name := range []string{"Netflix", "TikTok", "Spotify"} {
		if !found[name] {
			t.Fatalf("unlockCheckers 缺少 %s", name)
		}
	}
}

// ---------- 评分权重可配 ----------

func TestScoreWeightsApplied(t *testing.T) {
	w := ScoreWeights{Availability: 40, Latency: 30, Speed: 20, IPQ: 10}
	// 可用性满分 40：全目标通过
	ns := NodeScore{Alive: true}
	// 直接复用折算公式验证比例
	ns.ScoreAvail = w.Availability * 2 / 2
	ns.ScoreLat = w.Latency * 1
	ns.ScoreSpeed = w.Speed * 1
	ns.ScoreIPQ = w.IPQ * 0.5
	if ns.ScoreAvail != 40 || ns.ScoreLat != 30 || ns.ScoreSpeed != 20 || ns.ScoreIPQ != 5 {
		t.Fatalf("权重折算: %+v", ns)
	}
	// 报告模板动态满分
	body, err := RenderReport([]NodeScore{ns}, 1, w)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "可用性(40)") || !strings.Contains(s, "IP质量/10") {
		t.Fatalf("报告表头应使用自定义权重: 未命中")
	}
}

// ---------- 上传测速 ----------

func TestMeasureUpload(t *testing.T) {
	var got int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		atomic.AddInt64(&got, n)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	old := speedUpEndpoint
	speedUpEndpoint = srv.URL
	defer func() { speedUpEndpoint = old }()

	r := MeasureUpload(context.Background(), Direct, 1<<20, 3*time.Second)
	if r.Err != "" {
		t.Fatalf("upload err: %s", r.Err)
	}
	if r.UpMbps <= 0 || r.BytesRead == 0 {
		t.Fatalf("upload result: %+v", r)
	}
	if got != r.BytesRead {
		t.Fatalf("server got %d bytes, client sent %d", got, r.BytesRead)
	}
}

// ---------- 订阅清单(serve API 端到端) ----------

func TestServeSubsAPI(t *testing.T) {
	dir := t.TempDir()
	// 本地 Clash YAML,两个节点(离线可解析)
	subFile := filepath.Join(dir, "sub.yaml")
	os.WriteFile(subFile, []byte("proxies:\n  - {name: 节点A, type: http, server: 1.2.3.4, port: 8080}\n  - {name: 节点B, type: socks5, server: 5.6.7.8, port: 1080}\n"), 0o644)

	subs, err := loadSubs(filepath.Join(dir, "subs.json"))
	if err != nil {
		t.Fatal(err)
	}
	targets, _ := loadTargets(filepath.Join(dir, "targets.json"))
	loader := newNodeLoader(subs)
	srv := httptest.NewServer(buildMux(serveDeps{
		targets: targets,
		subs:    subs,
		jobs:    newJobManager(),
		timeout: 2 * time.Second,
		conc:    2,
		nodes:   loader.Get,
		reload:  func() { loader.Reload(context.Background()) },
	}))
	defer srv.Close()

	// 添加订阅 -> 节点加载完成
	resp, err := http.Post(srv.URL+"/api/subs", "application/json",
		strings.NewReader(fmt.Sprintf(`{"url":%q}`, subFile)))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("add sub: %v %v", err, resp)
	}
	var ar struct {
		Sub   Sub  `json:"sub"`
		Added bool `json:"added"`
	}
	json.NewDecoder(resp.Body).Decode(&ar)
	resp.Body.Close()
	if !ar.Added || ar.Sub.ID == "" {
		t.Fatalf("bad add resp: %+v", ar)
	}
	// 重复添加按 URL 去重
	resp, _ = http.Post(srv.URL+"/api/subs", "application/json", strings.NewReader(fmt.Sprintf(`{"url":%q}`, subFile)))
	var ar2 struct{ Added bool }
	json.NewDecoder(resp.Body).Decode(&ar2)
	resp.Body.Close()
	if ar2.Added {
		t.Fatal("重复订阅应跳过")
	}

	waitFor := func(cond func(s map[string]any) bool) map[string]any {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			r, _ := http.Get(srv.URL + "/api/state")
			var st map[string]any
			json.NewDecoder(r.Body).Decode(&st)
			r.Body.Close()
			if cond(st) {
				return st
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatal("等待状态超时")
		return nil
	}

	st := waitFor(func(s map[string]any) bool { return len(s["nodes"].([]any)) == 2 })
	subsList := st["subs"].([]any)
	if len(subsList) != 1 || int(subsList[0].(map[string]any)["nodes"].(float64)) != 2 {
		t.Fatalf("subs state: %v", subsList)
	}

	// 删除订阅 -> 节点清空
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/subs/"+ar.Sub.ID, nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil || dresp.StatusCode != 200 {
		t.Fatalf("delete sub: %v %v", err, dresp)
	}
	dresp.Body.Close()
	waitFor(func(s map[string]any) bool { return len(s["nodes"].([]any)) == 0 })
	if len(subs.All()) != 0 {
		t.Fatalf("删除后清单应为空: %+v", subs.All())
	}
}

// ---------- 本机网络体检 ----------

func TestIPSetEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"1.1.1.1"}, []string{"1.1.1.1"}, true},
		{[]string{"1.1.1.1", "2.2.2.2"}, []string{"2.2.2.2", "1.1.1.1"}, true},
		{[]string{"1.1.1.1"}, []string{"5.6.7.8"}, false},
		{[]string{"1.1.1.1"}, []string{"1.1.1.1", "2.2.2.2"}, false},
		{nil, nil, true},
	}
	for i, c := range cases {
		if got := ipSetEqual(c.a, c.b); got != c.want {
			t.Fatalf("case %d: %v", i, got)
		}
	}
}

func TestCheckSites(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	old := localCNSites
	localCNSites = []struct{ Name, URL string }{
		{"好站", ok.URL},
		{"坏站", fmt.Sprintf("http://127.0.0.1:%d/", dead)},
	}
	defer func() { localCNSites = old }()

	out := checkSites(context.Background(), localCNSites)
	if len(out) != 2 {
		t.Fatalf("sites: %+v", out)
	}
	byName := map[string]SiteCheck{}
	for _, s := range out {
		byName[s.Name] = s
	}
	if !byName["好站"].OK || byName["好站"].Status != 200 {
		t.Fatalf("好站: %+v", byName["好站"])
	}
	if byName["坏站"].OK || byName["坏站"].Err == "" {
		t.Fatalf("坏站应失败: %+v", byName["坏站"])
	}
}

func TestLocalCheckAPI(t *testing.T) {
	// 注入本地站点避免外网依赖;出口 IP/DNS 部分允许失败,只验形状
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	oldCN, oldGL := localCNSites, localGlobalSites
	localCNSites = []struct{ Name, URL string }{{"本地好站", ok.URL}}
	localGlobalSites = []struct{ Name, URL string }{{"本地好站2", ok.URL}}
	defer func() { localCNSites, localGlobalSites = oldCN, oldGL }()

	targets, _ := loadTargets(t.TempDir() + "/targets.json")
	srv := httptest.NewServer(buildMux(serveDeps{targets: targets, jobs: newJobManager(), timeout: 2 * time.Second, conc: 2}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/local", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("local: %v %v", err, resp)
	}
	var r LocalCheckResult
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if r.Domestic == nil || r.Foreign == nil {
		t.Fatalf("perspectives nil: %+v", r)
	}
	if len(r.Domestic.Sites) != 1 || !r.Domestic.Sites[0].OK {
		t.Fatalf("domestic sites: %+v", r.Domestic.Sites)
	}
	if r.Domestic.Ping == nil || r.Foreign.Ping == nil {
		t.Fatal("ping stats missing")
	}
}

// ---------- IP 查询缓存 TTL 与 Fresh 语义 ----------

type countingSource struct{ calls *int }

func (c countingSource) Name() string { return "counting" }
func (c countingSource) Lookup(_ context.Context, _ Tunnel, ip string) (*IPInfo, error) {
	*c.calls++
	return &IPInfo{Query: ip, Country: "测试国", Status: "success"}, nil
}

func TestLookupCacheTTLAndFresh(t *testing.T) {
	old := ipQualitySources
	var calls int
	ipQualitySources = []IPQualitySource{countingSource{&calls}}
	defer func() { ipQualitySources = old }()
	lookupCache = sync.Map{} // 测试间隔离

	ctx := context.Background()
	// 批量语义:同 key 二次查询命中缓存
	if _, err := LookupIP(ctx, Direct, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupIP(ctx, Direct, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("缓存应吸收第二次查询: calls=%d", calls)
	}
	// Fresh 语义:绕过缓存强制查询
	if _, err := LookupIPFresh(ctx, Direct, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Fresh 应绕过缓存: calls=%d", calls)
	}
	// TTL 过期:回填一个过期条目后,LookupIP 应重新查询
	lookupCache.Store("direct|1.1.1.1", &lookupEntry{info: &IPInfo{Status: "success"}, at: time.Now().Add(-10 * time.Minute)})
	if _, err := LookupIP(ctx, Direct, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("过期条目应重查: calls=%d", calls)
	}
}
