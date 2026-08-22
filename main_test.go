package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 订阅与 URI 解析 ----------

func TestParseSSSIP002(t *testing.T) {
	m, err := uriToNodeMap("ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:testpass")) + "@1.2.3.4:8388#%E9%A6%99%E6%B8%AF01")
	if err != nil {
		t.Fatal(err)
	}
	if m["type"] != "ss" || m["server"] != "1.2.3.4" || m["port"] != 8388 {
		t.Fatalf("bad ss map: %v", m)
	}
	if m["cipher"] != "aes-128-gcm" || m["password"] != "testpass" {
		t.Fatalf("bad ss auth: %v", m)
	}
	if m["name"] != "香港01" {
		t.Fatalf("bad ss name: %v", m["name"])
	}
}

func TestParseSSLegacy(t *testing.T) {
	legacy := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw@5.6.7.8:443"))
	m, err := uriToNodeMap("ss://" + legacy + "#old")
	if err != nil {
		t.Fatal(err)
	}
	if m["server"] != "5.6.7.8" || m["port"] != 443 || m["cipher"] != "aes-256-gcm" || m["password"] != "pw" {
		t.Fatalf("bad legacy ss: %v", m)
	}
}

func TestParseVmess(t *testing.T) {
	cfg := map[string]any{
		"v": "2", "ps": "vm节点", "add": "9.9.9.9", "port": 443,
		"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "aid": 0, "scy": "auto",
		"net": "ws", "path": "/path", "host": "cdn.example.com", "tls": "tls",
	}
	b, _ := json.Marshal(cfg)
	m, err := uriToNodeMap("vmess://" + base64.StdEncoding.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	if m["type"] != "vmess" || m["server"] != "9.9.9.9" || m["port"] != 443 || m["uuid"] != cfg["id"] {
		t.Fatalf("bad vmess: %v", m)
	}
	if m["tls"] != true || m["network"] != "ws" {
		t.Fatalf("bad vmess tls/net: %v", m)
	}
	ws := m["ws-opts"].(map[string]any)
	if ws["path"] != "/path" {
		t.Fatalf("bad ws path: %v", ws)
	}
}

func TestParseVlessTrojanHysteria2(t *testing.T) {
	m, err := uriToNodeMap("vless://uuid-123@vl.example.com:443?security=reality&sni=s.example.com&pbk=KEY&sid=ab&type=grpc&serviceName=gs&fp=chrome#vl")
	if err != nil {
		t.Fatal(err)
	}
	if m["type"] != "vless" || m["uuid"] != "uuid-123" {
		t.Fatalf("bad vless: %v", m)
	}
	if _, has := m["flow"]; has {
		t.Fatalf("flow 应缺省: %v", m)
	}
	if m["servername"] != "s.example.com" || m["client-fingerprint"] != "chrome" {
		t.Fatalf("bad vless tls: %v", m)
	}
	if m["network"] != "grpc" {
		t.Fatalf("bad vless net: %v", m)
	}

	m, err = uriToNodeMap("trojan://pass123@tj.example.com:443?sni=t.example.com#tj")
	if err != nil {
		t.Fatal(err)
	}
	if m["type"] != "trojan" || m["password"] != "pass123" || m["servername"] != "t.example.com" {
		t.Fatalf("bad trojan: %v", m)
	}

	m, err = uriToNodeMap("hy2://authpass@h2.example.com:8443?sni=h.example.com&insecure=1&obfs=salamander&obfs-password=ob#hy")
	if err != nil {
		t.Fatal(err)
	}
	if m["type"] != "hysteria2" || m["password"] != "authpass" || m["sni"] != "h.example.com" || m["skip-cert-verify"] != true {
		t.Fatalf("bad hy2: %v", m)
	}
}

func TestParseSubscriptionContent(t *testing.T) {
	// Clash YAML
	yaml := "proxies:\n  - name: a\n    type: ss\n    server: 1.1.1.1\n    port: 80\n    cipher: aes-128-gcm\n    password: x\n"
	maps, err := parseSubscriptionContent([]byte(yaml))
	if err != nil || len(maps) != 1 || maps[0]["name"] != "a" {
		t.Fatalf("yaml parse: %v %v", maps, err)
	}
	// base64 的 v2ray 列表
	uri := "socks5://u:p@2.2.2.2:1080#s1\nhttp://3.3.3.3:8080#h1\n"
	b64 := base64.StdEncoding.EncodeToString([]byte(uri))
	maps, err = parseSubscriptionContent([]byte(b64))
	if err != nil || len(maps) != 2 {
		t.Fatalf("v2ray parse: %v %v", maps, err)
	}
	if maps[0]["type"] != "socks5" || maps[1]["type"] != "http" {
		t.Fatalf("bad v2ray maps: %v", maps)
	}
}

// ---------- 输出工具 ----------

func TestDispWidth(t *testing.T) {
	if dispWidth("abc") != 3 {
		t.Fatal("ascii width")
	}
	if dispWidth("香港01") != 6 { // 2+2+1+1
		t.Fatalf("cjk width: %d", dispWidth("香港01"))
	}
}

// ---------- DNS 编解码 ----------

func TestDNSCodec(t *testing.T) {
	q := buildDNSQuery("www.example.com", 1)
	if q[0] != 0x12 || len(q) < 20 {
		t.Fatalf("bad query: %x", q[:4])
	}
	// 构造一个响应：header + question + 1 个 A 记录
	resp := append([]byte{}, q[:12]...)
	resp[2] = 0x81 // QR=1
	resp[6], resp[7] = 0, 1 // ANCOUNT
	resp = append(resp, q[12:]...) // question 原样
	resp = append(resp, 0xc0, 0x0c) // name 指针
	resp = append(resp, 0, 1, 0, 1) // A IN
	resp = append(resp, 0, 0, 0, 60) // TTL
	resp = append(resp, 0, 4) // RDLENGTH
	resp = append(resp, 93, 184, 216, 34) // 93.184.216.34
	addrs, ttl := parseDNSAnswers(resp)
	if len(addrs) != 1 || addrs[0] != "93.184.216.34" || ttl != 60 {
		t.Fatalf("bad parse: %v %d", addrs, ttl)
	}
}

func TestSplitHostPortDefault(t *testing.T) {
	cases := map[string]string{
		"https://a.com/x":        "a.com:443",
		"http://a.com":           "a.com:80",
		"a.com:8443":             "a.com:8443",
		"https://a.com:9443/x?y": "a.com:9443",
	}
	for in, want := range cases {
		got, err := SplitHostPortDefault(in)
		if err != nil || got != want {
			t.Fatalf("%s -> %s (want %s, err %v)", in, got, want, err)
		}
	}
}

// ---------- 隧道端到端 ----------

func TestHTTPCheckDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	r := HTTPCheck(context.Background(), Direct, srv.URL, 3*time.Second)
	if !r.OK || r.Status != 200 || r.TotalMs <= 0 {
		t.Fatalf("direct check: %+v", r)
	}
}

func TestHTTPCheckUnreachable(t *testing.T) {
	// 找一个必然关闭的端口
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	r := HTTPCheck(context.Background(), Direct, fmt.Sprintf("http://127.0.0.1:%d/", port), time.Second)
	if r.OK || r.Err == "" {
		t.Fatalf("should fail: %+v", r)
	}
}

// 最小 HTTP 正向代理（支持 CONNECT 隧道），验证 mihomo 节点链路。
func tinyForwardProxy(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect {
			dst, err := net.DialTimeout("tcp", req.Host, 3*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				dst.Close()
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			client, rwc, err := hj.Hijack()
			if err != nil {
				dst.Close()
				return
			}
			defer client.Close()
			if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
				dst.Close()
				return
			}
			done := make(chan struct{}, 2)
			go func() {
				io.Copy(dst, rwc.Reader)
				dst.Close()
				done <- struct{}{}
			}()
			io.Copy(client, dst)
			dst.Close()
			<-done
			return
		}
		// 非 CONNECT：绝对 URI 转发
		u := req.URL
		if u == nil || u.Host == "" {
			http.Error(w, "no host", http.StatusBadRequest)
			return
		}
		if u.Scheme == "" {
			u.Scheme = "http"
		}
		outReq, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), nil)
		if err != nil {
			http.Error(w, "build: "+err.Error(), http.StatusBadGateway)
			return
		}
		outReq.Header = req.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, "roundtrip: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
}

func TestTunnelViaHTTPProxyNode(t *testing.T) {
	// 目标站
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello-from-target")
	}))
	defer target.Close()
	// 本地转发代理
	proxy := tinyForwardProxy(t)
	defer proxy.Close()

	// 从代理地址提取 host:port
	pparts := strings.Split(strings.TrimPrefix(proxy.URL, "http://"), ":")
	pHost, pPort := pparts[0], pparts[1]
	var port int
	fmt.Sscanf(pPort, "%d", &port)

	node, err := ParseNode(map[string]any{
		"name": "本地测试代理", "type": "http", "server": pHost, "port": port,
	})
	if err != nil {
		t.Fatalf("ParseNode: %v", err)
	}
	tun, err := newProxyTunnel(node)
	if err != nil {
		t.Fatal(err)
	}
	r := HTTPCheck(context.Background(), tun, target.URL, 5*time.Second)
	if !r.OK || r.Status != 200 {
		t.Fatalf("via-proxy check: %+v", r)
	}
	if r.Node != "本地测试代理" || r.NodeType != "http" {
		t.Fatalf("bad tunnel meta: %+v", r)
	}
}

// ---------- 评分 ----------

func TestRenderReport(t *testing.T) {
	nodes := []NodeScore{
		{Node: "n1", Type: "ss", Alive: true, Total: 88, ScoreAvail: 20, ScoreLat: 25, ScoreSpeed: 28, ScoreIPQ: 15},
		{Node: "n2", Type: "vmess", Alive: false, Total: 0},
	}
	body, err := RenderReport(nodes, 1, DefaultScoreWeights())
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"netscope 综合体检报告", "n1", "n2", "Top 1", "88"} {
		if !strings.Contains(s, want) {
			t.Fatalf("report missing %q", want)
		}
	}
	if !strings.Contains(s, "<style>") || strings.Contains(s, "src=") {
		t.Fatal("报告必须是自包含单文件")
	}
}

// ---------- serve API 端到端 ----------

func TestServeAPI(t *testing.T) {
	// 两个目标：一个正常，一个必挂
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fine")
	}))
	defer okSrv.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	deadURL := fmt.Sprintf("http://127.0.0.1:%d/", deadPort)

	dir := t.TempDir()
	ts, err := loadTargets(dir + "/targets.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(buildMux(serveDeps{
		targets:   ts,
		jobs:      newJobManager(),
		timeout:   2 * time.Second,
		conc:      2,
		nodes:     func() []Tunnel { return nil },
	}))
	defer srv.Close()

	// 添加两个目标
	for _, u := range []string{okSrv.URL, deadURL} {
		resp, err := http.Post(srv.URL+"/api/targets", "application/json",
			strings.NewReader(fmt.Sprintf(`{"url":%q}`, u)))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("add target %s: %v %v", u, err, resp)
		}
		var tg Target
		json.NewDecoder(resp.Body).Decode(&tg)
		resp.Body.Close()
		if tg.ID == "" || tg.URL != u {
			t.Fatalf("bad target: %+v", tg)
		}
	}
	// state
	resp, _ := http.Get(srv.URL + "/api/state")
	var st struct {
		Targets []Target         `json:"targets"`
		Reports []map[string]any `json:"reports"`
		Nodes   []map[string]string `json:"nodes"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if len(st.Targets) != 2 {
		t.Fatalf("state targets: %+v", st.Targets)
	}
	// 发起检测
	ids := []string{st.Targets[0].ID, st.Targets[1].ID}
	body, _ := json.Marshal(map[string]any{"ids": ids, "via": "direct"})
	jresp, err := http.Post(srv.URL+"/api/jobs", "application/json", strings.NewReader(string(body)))
	if err != nil || jresp.StatusCode != 200 {
		t.Fatalf("start job: %v %v", err, jresp)
	}
	var jr struct{ ID string }
	json.NewDecoder(jresp.Body).Decode(&jr)
	jresp.Body.Close()

	// 轮询等待完成
	deadline := time.Now().Add(10 * time.Second)
	var job checkJob
	for time.Now().Before(deadline) {
		resp, _ = http.Get(srv.URL + "/api/jobs/" + jr.ID)
		json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if job.Finished {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !job.Finished || job.Done != 2 {
		t.Fatalf("job not finished: %+v", job)
	}
	okCount := 0
	for _, it := range job.Items {
		switch r := it.Result.(type) {
		case *CheckResult:
			if r.OK {
				okCount++
			}
		case map[string]any: // 经 JSON 往返后
			if r["ok"] == true {
				okCount++
			}
		}
	}
	if okCount != 1 {
		t.Fatalf("expect 1 ok: %+v", job.Items)
	}
	// 删除目标
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/targets/"+st.Targets[0].ID, nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil || dresp.StatusCode != 200 {
		t.Fatalf("delete: %v %v", err, dresp)
	}
	dresp.Body.Close()
	if len(ts.All()) != 1 {
		t.Fatalf("after delete: %+v", ts.All())
	}
}
