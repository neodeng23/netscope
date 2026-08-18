package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"time"
)

// PingStats 是 N 次 TCP ping 的统计。
type PingStats struct {
	Node   string  `json:"node"`
	Target string  `json:"target"`
	Sent   int     `json:"sent"`
	Recv   int     `json:"recv"`
	Loss   float64 `json:"lossPct"`
	MinMs  float64 `json:"minMs"`
	AvgMs  float64 `json:"avgMs"`
	MaxMs  float64 `json:"maxMs"`
	Jitter float64 `json:"jitterMs"`
	Err    string  `json:"err,omitempty"`
}

// TCPPing 经隧道对 host:port 做 count 次 TCP 连接测量。
func TCPPing(ctx context.Context, t Tunnel, addr string, count int, timeout, interval time.Duration) PingStats {
	st := PingStats{Node: t.Name(), Target: addr, Sent: count}
	var rtts []float64
	var lastErr string
loop:
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			st.Sent = i
			break
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		conn, err := t.DialContext(cctx, "tcp", addr)
		elapsed := float64(time.Since(start).Microseconds()) / 1000.0
		cancel()
		if err != nil {
			lastErr = cleanErr(err)
		} else {
			conn.Close()
			rtts = append(rtts, elapsed)
		}
		if i < count-1 && interval > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				st.Sent = i + 1
				break loop
			}
		}
	}
	st.Recv = len(rtts)
	if st.Recv > st.Sent {
		st.Sent = st.Recv
	}
	if st.Sent > 0 {
		st.Loss = float64(st.Sent-st.Recv) / float64(st.Sent) * 100
	}
	if len(rtts) > 0 {
		st.MinMs, st.MaxMs = rtts[0], rtts[0]
		sum := 0.0
		for _, r := range rtts {
			sum += r
			st.MinMs = math.Min(st.MinMs, r)
			st.MaxMs = math.Max(st.MaxMs, r)
		}
		st.AvgMs = sum / float64(len(rtts))
		if len(rtts) > 1 {
			jsum := 0.0
			for i := 1; i < len(rtts); i++ {
				jsum += math.Abs(rtts[i] - rtts[i-1])
			}
			st.Jitter = jsum / float64(len(rtts)-1)
		}
	}
	if st.Recv == 0 {
		st.Err = lastErr
	}
	return st
}

// SplitHostPortDefault 从 URL 提取 host:port（https 默认 443，http 默认 80）。
func SplitHostPortDefault(target string) (string, error) {
	scheme := ""
	if i := strings.Index(target, "://"); i >= 0 {
		scheme = strings.ToLower(target[:i])
		target = target[i+3:]
	}
	if i := strings.IndexAny(target, "/?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "", fmt.Errorf("目标地址为空")
	}
	if !strings.Contains(target, ":") {
		switch scheme {
		case "http":
			target += ":80"
		case "", "https":
			target += ":443"
		default:
			return "", fmt.Errorf("不支持的 scheme: %q", scheme)
		}
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return "", err
	}
	return target, nil
}
