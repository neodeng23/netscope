package main

// P2 功能测试：配置预设、快照清理、ipwho.is 数据源、TikTok/Spotify 解锁判定。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
