package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadSubscriptions 拉取全部订阅并解析、去重，返回节点通道列表。
// source 可以是 http(s) 链接、本地文件路径或 file:// 路径。
func LoadSubscriptions(ctx context.Context, sources []string, include, exclude []string) ([]Tunnel, error) {
	var all []Tunnel
	seen := map[string]bool{}
	for _, src := range sources {
		maps, err := fetchSubscription(ctx, src)
		if err != nil {
			return nil, fmt.Errorf("订阅 %s: %w", src, err)
		}
		n := 0
		for _, m := range maps {
			name, _ := m["name"].(string)
			if !matchFilter(name, include, exclude) {
				continue
			}
			key := nodeKey(m)
			if seen[key] {
				continue
			}
			seen[key] = true
			p, err := ParseNode(m)
			if err != nil {
				continue // 非法/不支持的节点跳过，不中断整体
			}
			t, err := newProxyTunnel(p)
			if err != nil {
				continue
			}
			all = append(all, t)
			n++
		}
		Progress("订阅 %s：解析到 %d 个节点\n", src, n)
	}
	return all, nil
}

func matchFilter(name string, include, exclude []string) bool {
	for _, k := range include {
		if k != "" && !strings.Contains(name, k) {
			return false
		}
	}
	for _, k := range exclude {
		if k != "" && strings.Contains(name, k) {
			return false
		}
	}
	return true
}

func nodeKey(m map[string]any) string {
	return fmt.Sprintf("%v|%v|%v", m["type"], m["server"], m["port"])
}

var subUA = "clash.meta/1.19.3 netscope/0.1"

func fetchSubscription(ctx context.Context, src string) ([]map[string]any, error) {
	var data []byte
	var err error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		data, err = httpGetRetry(ctx, src, 2)
	} else {
		path := strings.TrimPrefix(src, "file://")
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	return parseSubscriptionContent(data)
}

func httpGetRetry(ctx context.Context, u string, retries int) ([]byte, error) {
	var lastErr error
	for i := 0; i <= retries; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", subUA)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if err != nil {
				lastErr = err
			} else if resp.StatusCode != 200 {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			} else {
				return b, nil
			}
		}
		if i < retries {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	return nil, lastErr
}

// parseSubscriptionContent 自动识别：Clash YAML / v2ray Base64 / v2ray 明文 URI 列表。
func parseSubscriptionContent(data []byte) ([]map[string]any, error) {
	if maps := parseClashYAML(data); len(maps) > 0 {
		return maps, nil
	}
	text := strings.TrimSpace(string(data))
	// 整体 base64 的订阅
	if !strings.Contains(text, "://") {
		if dec, err := b64Decode(text); err == nil {
			text = strings.TrimSpace(dec)
		}
	}
	if !strings.Contains(text, "://") {
		return nil, fmt.Errorf("无法识别的订阅格式（既非 Clash YAML 也非节点 URI 列表）")
	}
	var nodes []map[string]any
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if m, err := uriToNodeMap(line); err == nil && m != nil {
			nodes = append(nodes, m)
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("订阅中没有可解析的节点")
	}
	return nodes, nil
}

func parseClashYAML(data []byte) []map[string]any {
	var cfg struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil || len(cfg.Proxies) == 0 {
		return nil
	}
	return cfg.Proxies
}

// b64Decode 兼容 std / url-safe、有无 padding。
func b64Decode(s string) (string, error) {
	s = strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("base64 解码失败")
}

// uriToNodeMap 把一条节点 URI 转成 clash 风格 map。
func uriToNodeMap(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(u.Scheme) {
	case "ss":
		return parseSS(u)
	case "ssr":
		return parseSSR(u)
	case "vmess":
		return parseVmess(u)
	case "vless":
		return parseVless(u)
	case "trojan":
		return parseTrojan(u)
	case "hysteria2", "hy2":
		return parseHysteria2(u)
	case "tuic":
		return parseTuic(u)
	case "socks", "socks5":
		return parseSocks(u)
	case "http", "https":
		return parseHTTPProxy(u)
	}
	return nil, fmt.Errorf("不支持的协议 %q", u.Scheme)
}

// uriPayload 取 vmess/ssr 等「整段 base64 载荷」：url.Parse 会把它放进 Host。
func uriPayload(u *url.URL) string {
	if u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(u.Opaque, "//")
}

func fragName(u *url.URL) string {
	name, _ := url.QueryUnescape(u.Fragment)
	return strings.TrimSpace(name)
}

func atoiDefault(s string, d int) int {
	if s == "" {
		return d
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

// ---------- ss ----------

func parseSS(u *url.URL) (map[string]any, error) {
	// 形式1：ss://base64(method:pass@host:port)#name（整体 legacy，无 userinfo）
	if u.User == nil {
		dec, err := b64Decode(u.Host)
		if err != nil {
			return nil, err
		}
		u2, err := url.Parse("ss://" + dec)
		if err != nil {
			return nil, err
		}
		m, err := parseSS(u2)
		if err == nil && m != nil {
			if name := fragName(u); name != "" {
				m["name"] = name
			}
		}
		return m, err
	}
	host := u.Hostname()
	port := atoiDefault(u.Port(), 0)
	method, password := "", ""
	if pi := strings.LastIndex(u.User.String(), ":"); pi >= 0 {
		// 明文 sip002: method:password@host:port
		method = u.User.Username()
		password, _ = url.QueryUnescape(u.User.String()[pi+1:])
	} else {
		// base64(method:password)
		dec, err := b64Decode(u.User.Username())
		if err != nil {
			return nil, err
		}
		if i := strings.Index(dec, ":"); i >= 0 {
			method, password = dec[:i], dec[i+1:]
		} else {
			return nil, fmt.Errorf("ss 节点缺少密码")
		}
	}
	if port == 0 || host == "" {
		return nil, fmt.Errorf("ss 节点地址不完整")
	}
	m := map[string]any{
		"name": fragName(u), "type": "ss", "server": host, "port": port,
		"cipher": method, "password": password, "udp": true,
	}
	if q := u.Query(); q.Get("plugin") != "" {
		m["plugin"] = q.Get("plugin")
		if po := q.Get("plugin-opts"); po != "" {
			opts := map[string]any{}
			for _, kv := range strings.Split(po, ";") {
				if i := strings.Index(kv, "="); i > 0 {
					opts[kv[:i]] = kv[i+1:]
				}
			}
			m["plugin-opts"] = opts
		}
	}
	return m, nil
}

// ---------- ssr ----------

func parseSSR(u *url.URL) (map[string]any, error) {
	dec, err := b64Decode(uriPayload(u))
	if err != nil {
		return nil, err
	}
	main := dec
	query := ""
	if i := strings.Index(dec, "/?"); i >= 0 {
		main, query = dec[:i], dec[i+2:]
	}
	parts := strings.Split(main, ":")
	if len(parts) != 6 {
		return nil, fmt.Errorf("ssr 节点格式错误")
	}
	port := atoiDefault(parts[1], 0)
	password, _ := b64Decode(parts[5])
	q, _ := url.ParseQuery(query)
	gd := func(k string) string { v, _ := url.QueryUnescape(q.Get(k)); return v }
	remark, _ := b64Decode(strings.TrimPrefix(q.Get("remarks"), ""))
	m := map[string]any{
		"name": remark, "type": "ssr", "server": parts[0], "port": port,
		"cipher": parts[3], "password": password,
		"protocol":  parts[2],
		"obfs":      parts[4],
		"udp":       true,
	}
	if v := gd("protoparam"); v != "" {
		m["protocol-param"] = v
	}
	if v := gd("obfsparam"); v != "" {
		m["obfs-param"] = v
	}
	if remark == "" {
		m["name"] = fmt.Sprintf("%s:%d", parts[0], port)
	}
	return m, nil
}

// ---------- vmess ----------

func parseVmess(u *url.URL) (map[string]any, error) {
	dec, err := b64Decode(uriPayload(u))
	if err != nil {
		return nil, err
	}
	var v struct {
		Ps   string      `json:"ps"`
		Add  string      `json:"add"`
		Port any         `json:"port"`
		ID   string      `json:"id"`
		Aid  any         `json:"aid"`
		Scy  string      `json:"scy"`
		Net  string      `json:"net"`
		Type string      `json:"type"`
		Host string      `json:"host"`
		Path string      `json:"path"`
		TLS  string      `json:"tls"`
		Sni  string      `json:"sni"`
		Alpn any         `json:"alpn"`
		Fp   string      `json:"fp"`
	}
	if err := json.Unmarshal([]byte(dec), &v); err != nil {
		return nil, err
	}
	port := 0
	switch p := v.Port.(type) {
	case float64:
		port = int(p)
	case string:
		port = atoiDefault(p, 0)
	}
	if v.Add == "" || port == 0 || v.ID == "" {
		return nil, fmt.Errorf("vmess 节点信息不完整")
	}
	cipher := v.Scy
	if cipher == "" {
		cipher = "auto"
	}
	aid := "0"
	switch a := v.Aid.(type) {
	case float64:
		aid = strconv.Itoa(int(a))
	case string:
		aid = a
	}
	m := map[string]any{
		"name": v.Ps, "type": "vmess", "server": v.Add, "port": port,
		"uuid": v.ID, "alterId": atoiDefault(aid, 0), "cipher": cipher,
		"udp": true,
	}
	if v.TLS == "tls" || v.TLS == "reality" {
		m["tls"] = true
	}
	if v.Sni != "" {
		m["servername"] = v.Sni
	} else if v.Host != "" && v.TLS != "" {
		m["servername"] = v.Host
	}
	if v.Fp != "" {
		m["client-fingerprint"] = v.Fp
	}
	if alpn := anyToStrings(v.Alpn); len(alpn) > 0 {
		m["alpn"] = alpn
	}
	net := v.Net
	if net == "" {
		net = "tcp"
	}
	if net != "tcp" {
		m["network"] = net
	}
	switch net {
	case "ws":
		opts := map[string]any{"path": v.Path}
		if v.Host != "" {
			opts["headers"] = map[string]any{"Host": v.Host}
		}
		m["ws-opts"] = opts
	case "grpc":
		m["grpc-opts"] = map[string]any{"grpc-service-name": v.Path}
	case "h2", "http":
		m["h2-opts"] = map[string]any{"host": []string{v.Host}, "path": v.Path}
	}
	return m, nil
}

func anyToStrings(v any) []string {
	switch a := v.(type) {
	case string:
		if a == "" {
			return nil
		}
		return strings.Split(a, ",")
	case []any:
		var out []string
		for _, s := range a {
			if s, ok := s.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------- vless / trojan / hysteria2 / tuic（查询参数风格） ----------

func parseVless(u *url.URL) (map[string]any, error) {
	q := u.Query()
	port := atoiDefault(u.Port(), 0)
	if u.Hostname() == "" || port == 0 {
		return nil, fmt.Errorf("vless 节点地址不完整")
	}
	m := map[string]any{
		"name": fragName(u), "type": "vless", "server": u.Hostname(), "port": port,
		"uuid": u.User.Username(), "udp": true,
	}
	if sec := q.Get("security"); sec == "tls" || sec == "reality" {
		m["tls"] = true
		if sec == "reality" {
			m["reality-opts"] = map[string]any{
				"public-key": q.Get("pbk"),
			}
			if sid := q.Get("sid"); sid != "" {
				m["reality-opts"].(map[string]any)["short-id"] = sid
			}
		}
	}
	if sni := q.Get("sni"); sni != "" {
		m["servername"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		m["client-fingerprint"] = fp
	}
	if flow := q.Get("flow"); flow != "" {
		m["flow"] = flow
	}
	if alpn := q.Get("alpn"); alpn != "" {
		m["alpn"] = strings.Split(alpn, ",")
	}
	if net := q.Get("type"); net != "" && net != "tcp" {
		m["network"] = net
		switch net {
		case "ws":
			opts := map[string]any{"path": q.Get("path")}
			if h := q.Get("host"); h != "" {
				opts["headers"] = map[string]any{"Host": h}
			}
			m["ws-opts"] = opts
		case "grpc":
			m["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
		}
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
		m["skip-cert-verify"] = true
	}
	return m, nil
}

func parseTrojan(u *url.URL) (map[string]any, error) {
	q := u.Query()
	port := atoiDefault(u.Port(), 443)
	if u.Hostname() == "" {
		return nil, fmt.Errorf("trojan 节点地址不完整")
	}
	pass, _ := url.QueryUnescape(u.User.String())
	m := map[string]any{
		"name": fragName(u), "type": "trojan", "server": u.Hostname(), "port": port,
		"password": pass, "udp": true,
	}
	if sni := q.Get("sni"); sni != "" {
		m["servername"] = sni
	} else if sni := q.Get("peer"); sni != "" {
		m["servername"] = sni
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
		m["skip-cert-verify"] = true
	}
	if net := q.Get("type"); net != "" && net != "tcp" {
		m["network"] = net
		if net == "ws" {
			opts := map[string]any{"path": q.Get("path")}
			if h := q.Get("host"); h != "" {
				opts["headers"] = map[string]any{"Host": h}
			}
			m["ws-opts"] = opts
		}
	}
	return m, nil
}

func parseHysteria2(u *url.URL) (map[string]any, error) {
	q := u.Query()
	port := atoiDefault(u.Port(), 443)
	if u.Hostname() == "" {
		return nil, fmt.Errorf("hysteria2 节点地址不完整")
	}
	auth, _ := url.QueryUnescape(u.User.String())
	m := map[string]any{
		"name": fragName(u), "type": "hysteria2", "server": u.Hostname(), "port": port,
		"password": auth,
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if q.Get("insecure") == "1" {
		m["skip-cert-verify"] = true
	}
	if obfs := q.Get("obfs"); obfs != "" {
		m["obfs"] = obfs
		if op := q.Get("obfs-password"); op != "" {
			m["obfs-password"] = op
		}
	}
	return m, nil
}

func parseTuic(u *url.URL) (map[string]any, error) {
	q := u.Query()
	port := atoiDefault(u.Port(), 443)
	if u.Hostname() == "" {
		return nil, fmt.Errorf("tuic 节点地址不完整")
	}
	pass, _ := url.QueryUnescape(u.User.String())
	m := map[string]any{
		"name": fragName(u), "type": "tuic", "server": u.Hostname(), "port": port,
		"uuid": u.User.Username(), "password": pass,
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if cc := q.Get("congestion_control"); cc != "" {
		m["congestion-controller"] = cc
	}
	if alpn := q.Get("alpn"); alpn != "" {
		m["alpn"] = strings.Split(alpn, ",")
	}
	if q.Get("allow_insecure") == "1" {
		m["skip-cert-verify"] = true
	}
	return m, nil
}

func parseSocks(u *url.URL) (map[string]any, error) {
	port := atoiDefault(u.Port(), 1080)
	m := map[string]any{
		"name": fragName(u), "type": "socks5", "server": u.Hostname(), "port": port,
		"udp": true,
	}
	if pass, ok := u.User.Password(); ok {
		m["username"] = u.User.Username()
		m["password"] = pass
	}
	return m, nil
}

func parseHTTPProxy(u *url.URL) (map[string]any, error) {
	port := atoiDefault(u.Port(), 8080)
	m := map[string]any{
		"name": fragName(u), "type": "http", "server": u.Hostname(), "port": port,
	}
	if pass, ok := u.User.Password(); ok {
		m["username"] = u.User.Username()
		m["password"] = pass
	}
	return m, nil
}

// LoadTunnelsFromFile 供 serve 等场景从本地/远端订阅构建通道。
func LoadTunnelsFromFile(ctx context.Context, sub string) ([]Tunnel, error) {
	return LoadSubscriptions(ctx, []string{sub}, nil, nil)
}
