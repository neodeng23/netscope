package main

// 游戏平台连接检测测试：货币区/店面解析、本地端到端、API。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseStorefront(t *testing.T) {
	cases := []struct {
		url, storefront, region string
	}{
		{"https://store.playstation.com/en-tw/pages/latest", "en-tw", "台湾"},
		{"https://store.playstation.com/zh-hant-tw/pages/latest", "zh-hant-tw", "台湾"},
		{"https://store.playstation.com/en-us/pages/latest", "en-us", "美国"},
		{"https://store.playstation.com/ja-jp/", "ja-jp", "日本"},
		{"https://store.playstation.com/en-hk/", "en-hk", "香港"},
		{"https://store.playstation.com/xx-zz/", "xx-zz", "ZZ"},
	}
	for _, c := range cases {
		sf, rg, ok := parseStorefront(c.url)
		if !ok || sf != c.storefront || rg != c.region {
			t.Fatalf("%s -> %q/%q (want %s/%s)", c.url, sf, rg, c.storefront, c.region)
		}
	}
	if _, _, ok := parseStorefront("https://store.playstation.com/"); ok {
		t.Fatal("无路径不应解析出店面")
	}
}

func TestSteamCurrencyRegex(t *testing.T) {
	page := `<html><body><span itemprop="price" content="278.00"></span><meta itemprop="priceCurrency" content="ARS"></body></html>`
	m := steamCurrencyRe.FindStringSubmatch(page)
	if m == nil || m[1] != "ARS" {
		t.Fatalf("currency: %v", m)
	}
	if steamCurrencyRegion["ARS"] != "阿根廷" {
		t.Fatal("ARS 应映射阿根廷")
	}
}

func withGameEndpoints(t *testing.T, store *httptest.Server) (restore func()) {
	t.Helper()
	old := steamStoreURL
	steamStoreURL = store.URL
	return func() { steamStoreURL = old }
}

func TestCheckSteamLocal(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<meta itemprop="priceCurrency" content="JPY">`)
	}))
	defer store.Close()
	community := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer community.Close()
	restore := withGameEndpoints(t, store)
	defer restore()
	oldC := steamCommunityURL
	steamCommunityURL = community.URL
	defer func() { steamCommunityURL = oldC }()

	r := CheckSteam(context.Background(), Direct, 3*time.Second)
	if !r.Store.OK || !r.Community.OK {
		t.Fatalf("sites: %+v %+v", r.Store, r.Community)
	}
	if r.Currency != "JPY" || r.Region != "日本" {
		t.Fatalf("currency: %s %s", r.Currency, r.Region)
	}
	if r.Err != "" {
		t.Fatalf("err: %s", r.Err)
	}
}

func TestCheckPSNLocal(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/en-hk/pages/latest", 302)
			return
		}
		w.WriteHeader(200)
	}))
	defer store.Close()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(301)
	}))
	defer account.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer web.Close()

	oldStore, oldAccount, oldWeb := psnStoreURL, psnAccountURL, psnWebURL
	psnStoreURL, psnAccountURL, psnWebURL = store.URL, account.URL, web.URL
	defer func() { psnStoreURL, psnAccountURL, psnWebURL = oldStore, oldAccount, oldWeb }()

	r := CheckPSN(context.Background(), Direct, 3*time.Second)
	if !r.Store.OK || !r.Account.OK || !r.Web.OK {
		t.Fatalf("sites: %+v %+v %+v", r.Store, r.Account, r.Web)
	}
	if r.Storefront != "en-hk" || r.Region != "香港" {
		t.Fatalf("storefront: %s %s", r.Storefront, r.Region)
	}
	if !strings.Contains(r.Store.FinalURL, "/en-hk/") {
		t.Fatalf("final url: %s", r.Store.FinalURL)
	}
}

func TestGameAPI(t *testing.T) {
	// 本地端点:steam 商店(含货币)与 psn 商店(重定向店面)各自独立
	steamStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<meta itemprop="priceCurrency" content="KRW">`)
	}))
	defer steamStore.Close()
	psnStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/en-jp/pages/latest", 302)
			return
		}
		w.WriteHeader(200)
	}))
	defer psnStore.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer other.Close()

	oldS, oldC := steamStoreURL, steamCommunityURL
	oldP, oldA, oldW := psnStoreURL, psnAccountURL, psnWebURL
	steamStoreURL, steamCommunityURL = steamStore.URL, other.URL
	psnStoreURL, psnAccountURL, psnWebURL = psnStore.URL, other.URL, other.URL
	defer func() {
		steamStoreURL, steamCommunityURL = oldS, oldC
		psnStoreURL, psnAccountURL, psnWebURL = oldP, oldA, oldW
	}()

	dir := t.TempDir()
	targets, _ := loadTargets(dir + "/targets.json")
	srv := httptest.NewServer(buildMux(serveDeps{
		targets: targets,
		jobs:    newJobManager(),
		timeout: 3 * time.Second,
		conc:    2,
		nodes:   func() []Tunnel { return []Tunnel{Direct, namedTunnel{"节点甲"}} },
	}))
	defer srv.Close()

	// 单平台 × 直连
	resp, err := http.Post(srv.URL+"/api/game", "application/json",
		strings.NewReader(`{"platform":"steam","via":"direct"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("steam api: %v %v", err, resp)
	}
	var sc SteamCheck
	json.NewDecoder(resp.Body).Decode(&sc)
	resp.Body.Close()
	if sc.Currency != "KRW" || sc.Region != "韩国" || !sc.Store.OK || !sc.Community.OK {
		t.Fatalf("steam result: %+v", sc)
	}

	resp, _ = http.Post(srv.URL+"/api/game", "application/json",
		strings.NewReader(`{"platform":"psn","via":"节点甲"}`))
	var pc PSNCheck
	json.NewDecoder(resp.Body).Decode(&pc)
	resp.Body.Close()
	if pc.Storefront != "en-jp" || pc.Region != "日本" || !pc.Account.OK {
		t.Fatalf("psn result: %+v", pc)
	}

	// 非法平台
	resp, _ = http.Post(srv.URL+"/api/game", "application/json",
		strings.NewReader(`{"platform":"xbox"}`))
	if resp.StatusCode != 400 {
		t.Fatalf("platform 校验应 400: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 全节点(2 节点 × 2 平台)
	resp, err = http.Post(srv.URL+"/api/game/all", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("game/all: %v %v", err, resp)
	}
	var jr struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&jr)
	resp.Body.Close()

	deadline := time.Now().Add(15 * time.Second)
	var job checkJob
	for time.Now().Before(deadline) {
		resp, _ = http.Get(srv.URL + "/api/jobs/" + jr.ID)
		json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if job.Finished {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !job.Finished || job.Done != 4 {
		t.Fatalf("job: %+v", job)
	}
	if job.Kind != "game" {
		t.Fatalf("kind: %s", job.Kind)
	}
	steamN, psnN := 0, 0
	for _, it := range job.Items {
		if it.Node == "" {
			t.Fatal("game job 应记录节点名")
		}
		if it.Result == nil {
			t.Fatal("game item result 为空")
		}
		if it.Platform == "steam" {
			steamN++
		} else {
			psnN++
		}
	}
	if steamN != 2 || psnN != 2 {
		t.Fatalf("平台分布: steam=%d psn=%d", steamN, psnN)
	}
}
