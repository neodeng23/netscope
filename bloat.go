package main

// route bloat：bufferbloat 测试——满载下载时对近目标的延迟变化，判断家庭网络拥塞。
// 流程一次性完成：空闲基准延迟 -> 并发下载压满带宽并持续测延迟 -> 恢复期观察回落数据。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// BloatResult 是一次 bufferbloat 测试的结果。
type BloatResult struct {
	Target      string  `json:"target"`
	Mode        string  `json:"mode"` // icmp / tcp
	DurationS   float64 `json:"durationSec"`
	Streams     int     `json:"streams"`
	DownMbps    float64 `json:"downMbps"`
	IdleAvgMs   float64 `json:"idleAvgMs"`
	LoadedAvgMs float64 `json:"loadedAvgMs"`
	LoadedMaxMs float64 `json:"loadedMaxMs"`
	IncreaseMs  float64 `json:"increaseMs"` // 满载相对空闲的平均延迟增加
	Grade       string  `json:"grade"`
	RecoverS    float64 `json:"recoverSec"` // 停止下载后延迟回到基准的耗时；-1 表示 15s 内未恢复
	Err         string  `json:"err,omitempty"`
}

// bloatGrade 按平均延迟增量评级（参照 bufferbloat 测试的常见分档）。
func bloatGrade(inc float64) string {
	switch {
	case inc <= 5:
		return "A+ 极佳"
	case inc <= 30:
		return "A 良好"
	case inc <= 60:
		return "B 一般"
	case inc <= 200:
		return "C 拥塞明显"
	default:
		return "D 严重拥塞"
	}
}

// bloatPinger 封装单次延迟测量：ICMP 优先，无权限降级 TCP 握手。
type bloatPinger struct {
	mode    string
	ip      net.IP
	icmp    *icmpConn
	tcpPort int
	seq     uint16
}

func newBloatPinger(ip net.IP, tcpPort int) (*bloatPinger, error) {
	if c, err := dialICMP(ip); err == nil {
		return &bloatPinger{mode: "icmp", ip: ip, icmp: c}, nil
	}
	return &bloatPinger{mode: "tcp", ip: ip, tcpPort: tcpPort}, nil
}

func (p *bloatPinger) Close() {
	if p.icmp != nil {
		p.icmp.Close()
	}
}

func (p *bloatPinger) Ping(timeout time.Duration) (float64, bool) {
	if p.mode == "icmp" {
		p.seq++
		start := time.Now()
		if err := p.icmp.SendEcho(p.ip, uint16(os.Getpid()&0xffff), p.seq); err != nil {
			return 0, false
		}
		for {
			msg, err := p.icmp.Recv(timeout)
			if err != nil {
				return 0, false
			}
			if msg.Type == 0 && msg.Seq == p.seq {
				return float64(time.Since(start).Microseconds()) / 1000.0, true
			}
		}
	}
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", net.JoinHostPort(p.ip.String(), fmt.Sprint(p.tcpPort)))
	if err != nil {
		return 0, false
	}
	conn.Close()
	return float64(time.Since(start).Microseconds()) / 1000.0, true
}

// BloatTest 执行 bufferbloat 测试（全部经本机直连）。
func BloatTest(ctx context.Context, host string, tcpPort int, dur time.Duration, streams, count int) BloatResult {
	r := BloatResult{Target: host, DurationS: dur.Seconds(), Streams: streams}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 || ips[0].To4() == nil {
		r.Err = fmt.Sprintf("解析 %s 失败或非 IPv4", host)
		return r
	}
	pinger, err := newBloatPinger(ips[0].To4(), tcpPort)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer pinger.Close()
	r.Mode = pinger.mode

	avg := func(xs []float64) float64 {
		s := 0.0
		for _, x := range xs {
			s += x
		}
		return s / float64(len(xs))
	}

	// 1) 空闲基准
	Progress("测量空闲基准延迟（%d 次，%s 模式）…\n", count, r.Mode)
	var idle []float64
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			r.Err = "中断"
			return r
		}
		if v, ok := pinger.Ping(2 * time.Second); ok {
			idle = append(idle, v)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(idle) == 0 {
		r.Err = "空闲期延迟全部失败（目标不可达？）"
		return r
	}
	r.IdleAvgMs = avg(idle)

	// 2) 满载：并发下载 + 持续 ping
	Progress("满载下载 %v × %d 流，同时测延迟…\n", dur, streams)
	loadCtx, stopLoad := context.WithTimeout(ctx, dur)
	defer stopLoad()
	tr := &http.Transport{} // 零值即本机直连
	defer tr.CloseIdleConnections()
	var wg sync.WaitGroup
	var total atomic.Int64
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runBloatLoad(loadCtx, tr, &total)
		}()
	}
	var loaded []float64
	for loadCtx.Err() == nil && ctx.Err() == nil {
		if v, ok := pinger.Ping(2 * time.Second); ok {
			loaded = append(loaded, v)
			if v > r.LoadedMaxMs {
				r.LoadedMaxMs = v
			}
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-loadCtx.Done():
		}
	}
	wg.Wait()
	if len(loaded) == 0 {
		r.Err = "满载期延迟全部失败"
		return r
	}
	r.LoadedAvgMs = avg(loaded)
	r.DownMbps = float64(total.Load()) * 8 / dur.Seconds() / 1e6
	r.IncreaseMs = r.LoadedAvgMs - r.IdleAvgMs
	if r.IncreaseMs < 0 {
		r.IncreaseMs = 0
	}
	r.Grade = bloatGrade(r.IncreaseMs)

	// 3) 恢复期：等延迟回到接近空闲水平
	Progress("等待延迟恢复…\n")
	baseline := r.IdleAvgMs*1.5 + 10
	r.RecoverS = -1
	start := time.Now()
	for time.Since(start) < 15*time.Second && ctx.Err() == nil {
		if v, ok := pinger.Ping(2 * time.Second); ok && v <= baseline {
			r.RecoverS = time.Since(start).Seconds()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return r
}

// runBloatLoad 循环下载大块数据直到 ctx 结束，累计读取字节数。
func runBloatLoad(ctx context.Context, tr *http.Transport, total *atomic.Int64) {
	const chunk = 25 << 20
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s%d", speedDownURL, chunk), nil)
		if err != nil {
			return
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			return
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		total.Add(n)
	}
}

// cmdRouteBloat 实现 `route bloat`。
func cmdRouteBloat(ctx context.Context, args []string) int {
	fs := newFlagSet("route bloat")
	tcpPort := fs.Int("tcp-port", 53, "降级 TCP 模式时使用的端口（默认目标为 DNS 服务器）")
	durFlag := secsDur{12 * time.Second}
	fs.Var(&durFlag, "dur", "满载下载时长（裸数字按秒）")
	streams := fs.Int("streams", 3, "并发下载流数")
	count := fs.Int("n", 6, "空闲基准 ping 次数")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	host := "223.5.5.5"
	if len(pos) >= 1 {
		host = pos[0]
	}

	r := BloatTest(ctx, host, *tcpPort, durFlag.Duration, *streams, *count)
	headers := []string{"目标", "模式", "空闲avg(ms)", "满载avg(ms)", "满载max(ms)", "延迟增加(ms)", "评级", "恢复", "下载Mbps"}
	recover := ">15s 未恢复"
	if r.RecoverS >= 0 {
		recover = fmt.Sprintf("%.1fs", r.RecoverS)
	}
	row := []string{
		r.Target, r.Mode,
		f1(r.IdleAvgMs), f1(r.LoadedAvgMs), f1(r.LoadedMaxMs),
		f1(r.IncreaseMs), r.Grade, recover, f1(r.DownMbps),
	}
	if r.Err != "" {
		row = append(row, r.Err)
		headers = append(headers, "错误")
	}
	PrintTable(headers, [][]string{row})
	writeJSONIfSet(*jsonOut, r)
	return boolCode(r.Err == "")
}
