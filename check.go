package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// CheckResult 是一次「经隧道访问目标 URL」的检测结果。
type CheckResult struct {
	Node     string  `json:"node"`     // 通道名（direct 或节点名）
	NodeType string  `json:"nodeType"` // ss / vmess / direct ...
	Target   string  `json:"target"`
	OK       bool    `json:"ok"`
	Status   int     `json:"status"`
	ConnMs   float64 `json:"connMs"`   // 建连耗时（含 TLS）
	TotalMs  float64 `json:"totalMs"`  // 完整请求耗时
	Err      string  `json:"err,omitempty"`
	ExitIP   string  `json:"exitIp,omitempty"`
	Location string  `json:"location,omitempty"` // 出口归属地
	IPFlags  string  `json:"ipFlags,omitempty"`  // 代理/机房标记
	CheckedAt time.Time `json:"checkedAt"`
}

// dialTLSVia 构建经隧道的 TLS 拨号函数。
func dialTLSVia(t Tunnel, tlsCfg *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := t.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(addr)
		cfg := &tls.Config{}
		if tlsCfg != nil {
			cfg = tlsCfg.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		tc := tls.Client(raw, cfg)
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			raw.Close()
			return nil, err
		}
		return tc, nil
	}
}

// HTTPCheck 经隧道访问 target（GET），测量建连与总耗时，并回填出口 IP 归属地。
func HTTPCheck(ctx context.Context, t Tunnel, target string, timeout time.Duration) CheckResult {
	r := CheckResult{Node: t.Name(), NodeType: t.Type(), Target: target, CheckedAt: time.Now()}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	req.Header.Set("User-Agent", subUA)
	tr := &http.Transport{
		DialContext:     t.DialContext,
		DialTLSContext:  dialTLSVia(t, nil),
		IdleConnTimeout: 30 * time.Second,
	}
	var connDone time.Time
	trace := &httptrace.ClientTrace{
		ConnectDone: func(network, addr string, err error) {
			connDone = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			if connDone.IsZero() {
				connDone = time.Now()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	r.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
	if !connDone.IsZero() {
		r.ConnMs = float64(connDone.Sub(start).Microseconds()) / 1000.0
	}
	if err != nil {
		r.Err = cleanErr(err)
		tr.CloseIdleConnections()
		return r
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	tr.CloseIdleConnections()
	_ = body
	r.Status = resp.StatusCode
	r.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
	return r
}

func cleanErr(err error) string {
	s := err.Error()
	if i := len(s); i > 120 {
		s = s[:120]
	}
	return s
}

// FillExitIP 为检测结果补充出口 IP 与归属地（带缓存，每通道只查一次）。
func FillExitIP(ctx context.Context, t Tunnel, r *CheckResult) {
	info, err := LookupIP(ctx, t, "")
	if err != nil || info == nil {
		return
	}
	r.ExitIP = info.Query
	r.Location = info.Location()
	r.IPFlags = info.Flags()
}

// RunChecks 对 nodes × targets 并发检测，onItem 在每条结果产生时回调（可为 nil）。
func RunChecks(ctx context.Context, nodes []Tunnel, targets []string, timeout time.Duration, concurrency int, onItem func(CheckResult)) []CheckResult {
	var mu sync.Mutex
	var results []CheckResult
	var tasks []func(context.Context)
	for _, n := range nodes {
		n := n
		for _, tg := range targets {
			tg := tg
			tasks = append(tasks, func(ctx context.Context) {
				if ctx.Err() != nil {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				r := HTTPCheck(cctx, n, tg, timeout)
				FillExitIP(cctx, n, &r)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				if onItem != nil {
					onItem(r)
				}
			})
		}
	}
	RunParallel(ctx, concurrency, tasks)
	// 排序：可用优先，耗时升序
	sortChecks(results)
	return results
}

func sortChecks(rs []CheckResult) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			a, b := rs[j-1], rs[j]
			if (!a.OK && b.OK) || (a.OK == b.OK && a.TotalMs > b.TotalMs) {
				rs[j-1], rs[j] = b, a
			} else {
				break
			}
		}
	}
}
