package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- STUN 编解码 ----------

func TestSTUNParse(t *testing.T) {
	req := stunRequest()
	if typ := binary.BigEndian.Uint16(req[0:]); typ != stunBindingReq {
		t.Fatalf("bad request type: %x", typ)
	}
	if binary.BigEndian.Uint32(req[4:]) != stunMagicCookie {
		t.Fatal("bad magic cookie")
	}
	// 构造带 XOR-MAPPED-ADDRESS 的成功响应
	resp := make([]byte, 20+12)
	resp[0], resp[1] = 0x01, 0x01
	binary.BigEndian.PutUint16(resp[2:], 12)
	binary.BigEndian.PutUint32(resp[4:], stunMagicCookie)
	copy(resp[8:], req[8:])
	p := 20
	binary.BigEndian.PutUint16(resp[p:], stunXorMapped)
	binary.BigEndian.PutUint16(resp[p+2:], 8)
	resp[p+5] = 0x01 // IPv4
	binary.BigEndian.PutUint16(resp[p+6:], 443^uint16(stunMagicCookie>>16))
	for i := 0; i < 4; i++ {
		resp[p+8+i] = [4]byte{1, 2, 3, 4}[i] ^ stunCookieBytes[i]
	}
	addr, err := stunParseResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "1.2.3.4:443" {
		t.Fatalf("bad addr: %s", addr)
	}
	// 坏响应报错
	if _, err := stunParseResponse(resp[:10]); err == nil {
		t.Fatal("短报文应报错")
	}
	resp[0], resp[1] = 0x01, 0x11 // 错误响应类型
	if _, err := stunParseResponse(resp); err == nil {
		t.Fatal("错误类型应报错")
	}
}

// TestUDPProbeDirect 用本地 STUN 模拟服务器验证 direct 通道的 UDP 探测。
func TestUDPProbeDirect(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("UDP 不可用:", err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		_, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		resp := make([]byte, 20+12)
		resp[0], resp[1] = 0x01, 0x01
		binary.BigEndian.PutUint16(resp[2:], 12)
		binary.BigEndian.PutUint32(resp[4:], stunMagicCookie)
		copy(resp[8:], buf[8:20])
		p := 20
		binary.BigEndian.PutUint16(resp[p:], stunXorMapped)
		binary.BigEndian.PutUint16(resp[p+2:], 8)
		resp[p+5] = 0x01
		ua := from.(*net.UDPAddr)
		ip := ua.IP.To4()
		if ip == nil {
			ip = net.IPv4(127, 0, 0, 1).To4()
		}
		binary.BigEndian.PutUint16(resp[p+6:], uint16(ua.Port)^uint16(stunMagicCookie>>16))
		for i := 0; i < 4; i++ {
			resp[p+8+i] = ip[i] ^ stunCookieBytes[i]
		}
		pc.WriteTo(resp, from)
	}()
	addr := pc.LocalAddr().String()
	exit, rtt, err := UDPProbe(context.Background(), Direct, addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(exit, "127.0.0.1:") {
		t.Fatalf("bad exit addr: %s", exit)
	}
	if rtt < 0 {
		t.Fatalf("bad rtt: %v", rtt)
	}
}

// ---------- subscription-userinfo ----------

func TestParseUserInfo(t *testing.T) {
	var info SubInfo
	parseUserInfo("upload=1000; download=2000; total=10000; expire=1900000000", &info)
	if info.Upload != 1000 || info.Download != 2000 || info.Total != 10000 || !info.HasUsage {
		t.Fatalf("bad usage: %+v", info)
	}
	if info.Expire != 1900000000 {
		t.Fatalf("bad expire: %d", info.Expire)
	}
	// 毫秒时间戳的兼容
	var info2 SubInfo
	parseUserInfo("expire=1900000000000", &info2)
	if info2.Expire != 1900000000 {
		t.Fatalf("ms expire not normalized: %d", info2.Expire)
	}
	// 空头/坏值不崩
	var info3 SubInfo
	parseUserInfo("", &info3)
	parseUserInfo("garbage=x; total=abc", &info3)
	if info3.HasUsage || info3.Total != 0 {
		t.Fatalf("bad garbage handling: %+v", info3)
	}
}

// ---------- DNS TXT ----------

func TestParseDNSTXT(t *testing.T) {
	q := buildDNSQuery("test.example.com", 16)
	resp := append([]byte{}, q[:12]...)
	resp[2] = 0x81
	resp[6], resp[7] = 0, 2
	resp = append(resp, q[12:]...)
	appendTXT := func(s string) {
		resp = append(resp, 0xc0, 0x0c)
		resp = append(resp, 0, 16, 0, 1) // TXT IN
		resp = append(resp, 0, 0, 0, 60) // TTL
		payload := []byte(s)
		rd := append([]byte{byte(len(payload))}, payload...)
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(rd)))
		resp = append(resp, rd...)
	}
	appendTXT("1.2.3.4")
	appendTXT("5.6.7.0/24")
	txts := parseDNSTXT(resp)
	if len(txts) != 2 || txts[0] != "1.2.3.4" || txts[1] != "5.6.7.0/24" {
		t.Fatalf("bad txts: %v", txts)
	}
}

// ---------- IP 风险分 ----------

func TestIPRiskScore(t *testing.T) {
	clean := &IPInfo{Status: "success"}
	if clean.RiskScore() != 0 || clean.Flags() != "" {
		t.Fatalf("clean ip: %d %q", clean.RiskScore(), clean.Flags())
	}
	proxy := &IPInfo{Status: "success", Proxy: true}
	if proxy.RiskScore() != 45 || proxy.Flags() != "代理" {
		t.Fatalf("proxy ip: %d %q", proxy.RiskScore(), proxy.Flags())
	}
	dc := &IPInfo{Status: "success", Hosting: true, Mobile: true}
	if dc.RiskScore() != 45 || dc.Flags() != "机房/移动" {
		t.Fatalf("dc ip: %d %q", dc.RiskScore(), dc.Flags())
	}
	unknown := &IPInfo{Status: "fail"}
	if unknown.RiskScore() != -1 {
		t.Fatalf("unknown ip: %d", unknown.RiskScore())
	}
}

// ---------- report diff ----------

func TestDiffSnapshots(t *testing.T) {
	old := rateSnapshot{Time: "t1", Nodes: []NodeScore{
		{Node: "a", Type: "ss", Alive: true, Total: 80, Ping: &PingStats{Recv: 5, AvgMs: 50}, Speed: &SpeedResult{DownMbps: 20}},
		{Node: "b", Type: "ss", Alive: true, Total: 60},
		{Node: "c", Type: "vmess"},
	}}
	cur := rateSnapshot{Time: "t2", Nodes: []NodeScore{
		{Node: "a", Type: "ss", Alive: true, Total: 90, Ping: &PingStats{Recv: 5, AvgMs: 40}},
		{Node: "c", Type: "vmess", Alive: true, Total: 70},
		{Node: "d", Type: "trojan", Alive: true, Total: 50},
	}}
	rows := DiffSnapshots(old, cur)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows: %+v", rows)
	}
	// 排序：新增 -> 保留（Δ升序）-> 移除
	byNode := map[string]DiffRow{}
	for _, r := range rows {
		byNode[r.Node] = r
	}
	if byNode["d"].Change != "added" || byNode["b"].Change != "removed" {
		t.Fatalf("bad change: %+v", byNode)
	}
	if byNode["a"].Change != "kept" || byNode["a"].Delta != 10 || byNode["a"].OldAvgMs != 50 || byNode["a"].NewAvgMs != 40 {
		t.Fatalf("bad kept a: %+v", byNode["a"])
	}
	if byNode["a"].NewDown != 0 || byNode["a"].OldDown != 20 {
		t.Fatalf("bad speed diff: %+v", byNode["a"])
	}
	if rows[0].Node != "d" || rows[len(rows)-1].Node != "b" {
		t.Fatalf("bad order: %+v", rows)
	}
}

// ---------- serve API：分组 / 导入导出 / 全节点任务 / 趋势 ----------

// namedTunnel 是换个名字的直连通道，测试多通道（全节点）任务用。
type namedTunnel struct{ name string }

func (n namedTunnel) Name() string   { return n.name }
func (n namedTunnel) Type() string   { return "direct" }
func (n namedTunnel) Server() string { return "" }
func (n namedTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}
func (n namedTunnel) SupportsUDP() bool { return true }
func (n namedTunnel) ListenPacket(ctx context.Context, addr string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, "udp", ":0")
}

func TestServeAPIGroups(t *testing.T) {
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
		nodes:     func() []Tunnel { return []Tunnel{Direct, namedTunnel{"节点甲"}} },
	}))
	defer srv.Close()

	post := func(path, body string) *http.Response {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		return resp
	}

	// 带分组添加
	resp := post("/api/targets", `{"url":"https://a.com","group":"工作"}`)
	var tg Target
	json.NewDecoder(resp.Body).Decode(&tg)
	resp.Body.Close()
	if tg.ID == "" || tg.Group != "工作" {
		t.Fatalf("bad target: %+v", tg)
	}

	// 修改分组
	resp = post("/api/targets/update", fmt.Sprintf(`{"id":%q,"group":"生活"}`, tg.ID))
	var up Target
	json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	if up.Group != "生活" {
		t.Fatalf("update failed: %+v", up)
	}

	// 导入（重复 URL 跳过）
	resp = post("/api/targets/import", `[{"url":"https://b.com","group":"生活"},{"url":"https://a.com"}]`)
	var imp struct{ Added int }
	json.NewDecoder(resp.Body).Decode(&imp)
	resp.Body.Close()
	if imp.Added != 1 {
		t.Fatalf("import added = %d", imp.Added)
	}

	// state 含分组
	resp, _ = http.Get(srv.URL + "/api/state")
	var st struct {
		Targets []Target `json:"targets"`
		Groups  []string `json:"groups"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if len(st.Targets) != 2 || len(st.Groups) != 1 || st.Groups[0] != "生活" {
		t.Fatalf("bad state: %+v %v", st.Targets, st.Groups)
	}

	// 全节点任务（nodes 返回两个通道，验证 item 记录节点）
	resp = post("/api/jobs", fmt.Sprintf(`{"ids":[%q],"via":"all"}`, tg.ID))
	var jr struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&jr)
	resp.Body.Close()
	if jr.ID == "" {
		t.Fatal("no job id")
	}
	deadline := time.Now().Add(10 * time.Second)
	var job checkJob
	for time.Now().Before(deadline) {
		resp, _ = http.Get(srv.URL + "/api/jobs/" + jr.ID)
		json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if job.Finished {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !job.Finished || len(job.Items) != 2 {
		t.Fatalf("bad all-node job: %+v", job)
	}
	gotNodes := map[string]bool{}
	for _, it := range job.Items {
		if it.Node == "" {
			t.Fatalf("multi-tunnel item missing node: %+v", it)
		}
		gotNodes[it.Node] = true
	}
	if len(gotNodes) != 2 || !gotNodes["direct"] || !gotNodes["节点甲"] {
		t.Fatalf("bad nodes: %v", gotNodes)
	}
	if !strings.Contains(job.Via, "全部节点") {
		t.Fatalf("bad job label: %s", job.Via)
	}

	// 导出
	resp, _ = http.Get(srv.URL + "/api/targets/export")
	var exported []Target
	json.NewDecoder(resp.Body).Decode(&exported)
	resp.Body.Close()
	if len(exported) != 2 {
		t.Fatalf("bad export: %+v", exported)
	}

}
