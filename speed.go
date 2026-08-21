package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SpeedResult 是经节点测速的结果。
type SpeedResult struct {
	Node       string  `json:"node"`
	DownMbps   float64 `json:"downMbps"`
	UpMbps     float64 `json:"upMbps,omitempty"`
	BytesRead  int64   `json:"bytesRead"`
	DurationS  float64 `json:"durationSec"`
	Err        string  `json:"err,omitempty"`
}

const (
	speedDownURL = "https://speed.cloudflare.com/__down?bytes="
	speedUpURL   = "https://speed.cloudflare.com/__up"
	maxSpeedSize = 200 << 20 // 单次下载上限 200MB
)

// MeasureSpeed 经隧道下载测速：最多下载 maxBytes 或持续 maxDur（先到为准）。
// 节点不可达或超时返回 Err。
func MeasureSpeed(ctx context.Context, t Tunnel, maxBytes int64, maxDur time.Duration) SpeedResult {
	r := SpeedResult{Node: t.Name()}
	if maxBytes <= 0 || maxBytes > maxSpeedSize {
		maxBytes = maxSpeedSize
	}
	if maxDur <= 0 {
		maxDur = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, maxDur+5*time.Second)
	defer cancel()

	tr := &http.Transport{
		DialContext:     t.DialContext,
		DialTLSContext:  dialTLSVia(t, &tls.Config{NextProtos: []string{"h2", "http/1.1"}}),
	}
	defer tr.CloseIdleConnections()

	// 预热一次小请求，排除握手时间
	warmReq, _ := http.NewRequestWithContext(ctx, "GET", speedDownURL+"1", nil)
	if warmResp, err := tr.RoundTrip(warmReq); err == nil {
		io.Copy(io.Discard, warmResp.Body)
		warmResp.Body.Close()
	}
	if ctx.Err() != nil {
		r.Err = "超时"
		return r
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s%d", speedDownURL, maxBytes), nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		r.Err = cleanErr(err)
		return r
	}
	defer resp.Body.Close()
	start := time.Now()
	deadline := start.Add(maxDur)
	buf := make([]byte, 64<<10)
	var total int64
	for {
		if ctx.Err() != nil || time.Now().After(deadline) || total >= maxBytes {
			break
		}
		n, err := resp.Body.Read(buf)
		total += int64(n)
		if err != nil {
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	r.BytesRead = total
	r.DurationS = elapsed
	if elapsed <= 0 || total == 0 {
		r.Err = "无数据"
		return r
	}
	r.DownMbps = float64(total) * 8 / elapsed / 1e6
	return r
}

// speedUpEndpoint 可在测试中替换为本地服务器。
var speedUpEndpoint = speedUpURL

// uploadReader 持续产出伪随机数据，直到达到字节上限或时限（返回 EOF 结束请求）。
type uploadReader struct {
	ctx      context.Context
	deadline time.Time
	max      int64
	sent     *int64
	buf      []byte
}

func (u *uploadReader) Read(p []byte) (int, error) {
	if u.ctx.Err() != nil || time.Now().After(u.deadline) || *u.sent >= u.max {
		return 0, io.EOF
	}
	n := copy(p, u.buf)
	*u.sent += int64(n)
	return n, nil
}

// MeasureUpload 经隧道上传测速：向 Cloudflare __up 持续 POST 随机数据。
func MeasureUpload(ctx context.Context, t Tunnel, maxBytes int64, maxDur time.Duration) SpeedResult {
	r := SpeedResult{Node: t.Name()}
	if maxBytes <= 0 || maxBytes > maxSpeedSize {
		maxBytes = maxSpeedSize
	}
	if maxDur <= 0 {
		maxDur = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, maxDur+5*time.Second)
	defer cancel()

	// 上传固定 HTTP/1.1：单连接分块发送，测得的发送量即真实吞吐，行为稳定。
	tr := &http.Transport{
		DialContext:    t.DialContext,
		DialTLSContext: dialTLSVia(t, &tls.Config{NextProtos: []string{"http/1.1"}}),
	}
	defer tr.CloseIdleConnections()

	payload := make([]byte, 64<<10)
	rand.Read(payload) // 随机内容，避免可压缩导致虚高

	var sent int64
	start := time.Now()
	deadline := start.Add(maxDur)
	body := &uploadReader{ctx: ctx, deadline: deadline, max: maxBytes, sent: &sent, buf: payload}
	req, err := http.NewRequestWithContext(ctx, "POST", speedUpEndpoint, body)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	req.ContentLength = -1 // 分块传输，长度未知
	resp, err := tr.RoundTrip(req)
	elapsed := time.Since(start).Seconds()
	if resp != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}
	if err != nil && sent == 0 {
		r.Err = cleanErr(err)
		return r
	}
	r.BytesRead = sent
	r.DurationS = elapsed
	if sent == 0 || elapsed <= 0 {
		r.Err = "无数据"
		return r
	}
	r.UpMbps = float64(sent) * 8 / elapsed / 1e6
	return r
}
