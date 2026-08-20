package main

import (
	"context"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// NodeScore 是单节点的综合评分明细。
type NodeScore struct {
	Node       string       `json:"node"`
	Type       string       `json:"type"`
	Server     string       `json:"server,omitempty"`
	Check      CheckResult  `json:"check"`
	Ping       *PingStats   `json:"ping,omitempty"`
	Speed      *SpeedResult `json:"speed,omitempty"`
	IP         *IPInfo      `json:"ip,omitempty"`
	Alive      bool         `json:"alive"`
	ScoreAvail float64      `json:"scoreAvail"` // 满分 20
	ScoreLat   float64      `json:"scoreLat"`   // 满分 30
	ScoreSpeed float64      `json:"scoreSpeed"` // 满分 30
	ScoreIPQ   float64      `json:"scoreIpq"`   // 满分 20
	Total      float64      `json:"total"`      // 0-100
}

// RateOptions 控制 sub rate 的行为。
type RateOptions struct {
	Targets     []string
	PingCount   int
	SpeedSize   int64
	SpeedDur    time.Duration
	Timeout     time.Duration
	Concurrency int
}

// RateNodes 对全部节点跑 check+ping+speed+ip 并评分。
func RateNodes(ctx context.Context, nodes []Tunnel, opt RateOptions, onDone func(NodeScore)) []NodeScore {
	var mu sync.Mutex
	var out []NodeScore
	var tasks []func(context.Context)
	for _, n := range nodes {
		n := n
		tasks = append(tasks, func(ctx context.Context) {
			ns := rateOne(ctx, n, opt)
			mu.Lock()
			out = append(out, ns)
			mu.Unlock()
			if onDone != nil {
				onDone(ns)
			}
		})
	}
	RunParallel(ctx, opt.Concurrency, tasks)
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}

func rateOne(ctx context.Context, n Tunnel, opt RateOptions) NodeScore {
	ns := NodeScore{Node: n.Name(), Type: n.Type(), Server: n.Server()}
	targets := opt.Targets
	if len(targets) == 0 {
		targets = []string{"https://www.gstatic.com/generate_204"}
	}
	// 1) 可用性（全部目标）
	okN := 0
	var first CheckResult
	for i, tg := range targets {
		cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
		r := HTTPCheck(cctx, n, tg, opt.Timeout)
		FillExitIP(cctx, n, &r)
		cancel()
		if i == 0 {
			first = r
		}
		if r.OK {
			okN++
		}
		if ctx.Err() != nil {
			break
		}
	}
	ns.Check = first
	ns.Alive = okN > 0
	ns.ScoreAvail = 20 * float64(okN) / float64(len(targets))
	if !ns.Alive {
		return ns // 死节点跳过后续昂贵检测
	}
	if ip, err := LookupIP(ctx, n, ""); err == nil {
		ns.IP = ip
	}
	// 2) 延迟
	addr, err := SplitHostPortDefault(targets[0])
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, opt.Timeout*time.Duration(opt.PingCount+2))
		st := TCPPing(pctx, n, addr, opt.PingCount, opt.Timeout, 200*time.Millisecond)
		cancel()
		ns.Ping = &st
		if st.Recv > 0 {
			// <=50ms 满分 30，>=600ms 0 分，线性
			x := (600 - st.AvgMs) / 550
			if x > 1 {
				x = 1
			}
			if x < 0 {
				x = 0
			}
			ns.ScoreLat = 30 * x
		}
	}
	// 3) 速度
	sp := MeasureSpeed(ctx, n, opt.SpeedSize, opt.SpeedDur)
	ns.Speed = &sp
	if sp.Err == "" && sp.DownMbps > 0 {
		x := sp.DownMbps / 50 // >=50Mbps 满分
		if x > 1 {
			x = 1
		}
		ns.ScoreSpeed = 30 * x
	}
	// 4) IP 质量：风险分折算（风险 0 满分 20，风险 100 为 0 分；未知给一半）
	ns.ScoreIPQ = 10
	if ns.IP != nil {
		if risk := ns.IP.RiskScore(); risk >= 0 {
			ns.ScoreIPQ = 20 - float64(risk)/5
		}
	}
	ns.Total = ns.ScoreAvail + ns.ScoreLat + ns.ScoreSpeed + ns.ScoreIPQ
	return ns
}

// ---------- HTML 报告 ----------

func defaultReportDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./reports"
	}
	return filepath.Join(home, ".netscope", "reports")
}

var tplFuncs = template.FuncMap{"add": func(a, b int) int { return a + b }}

var reportTpl = template.Must(template.New("report").Funcs(tplFuncs).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>netscope 体检报告 {{.Time}}</title>
<style>
  :root { --ok:#16a34a; --bad:#dc2626; --bar:#2563eb; --bg:#f8fafc; }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; background: var(--bg); color: #0f172a; padding: 24px; }
  .wrap { max-width: 1100px; margin: 0 auto; }
  h1 { font-size: 22px; margin-bottom: 4px; }
  .sub { color: #64748b; font-size: 13px; margin-bottom: 20px; }
  .cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px,1fr)); gap: 12px; margin-bottom: 28px; }
  .card { background: #fff; border-radius: 12px; padding: 14px 16px; box-shadow: 0 1px 3px rgba(0,0,0,.08); }
  .card .rank { font-size: 12px; color: #64748b; }
  .card .name { font-size: 15px; font-weight: 600; margin: 2px 0 8px; word-break: break-all; }
  .card .total { font-size: 26px; font-weight: 700; color: var(--bar); }
  .card .meta { font-size: 12px; color: #64748b; margin-top: 6px; line-height: 1.6; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 12px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,.08); font-size: 13px; }
  th, td { padding: 8px 10px; text-align: left; border-bottom: 1px solid #e2e8f0; white-space: nowrap; }
  th { background: #f1f5f9; font-weight: 600; }
  tr:hover td { background: #f8fafc; }
  .ok { color: var(--ok); font-weight: 600; }
  .bad { color: var(--bad); }
  .bar { display: inline-block; height: 6px; background: var(--bar); border-radius: 3px; vertical-align: middle; margin-right: 6px; }
  .barbg { display: inline-block; width: 80px; height: 6px; background: #e2e8f0; border-radius: 3px; vertical-align: middle; margin-left: 6px; position: relative; }
  .barbg i { position: absolute; left: 0; top: 0; bottom: 0; background: var(--bar); border-radius: 3px; }
  .tag { display: inline-block; font-size: 11px; background: #eef2ff; color: #4338ca; border-radius: 4px; padding: 1px 6px; margin-right: 4px; }
  footer { margin-top: 16px; color: #94a3b8; font-size: 12px; }
</style>
</head>
<body><div class="wrap">
<h1>🩺 netscope 综合体检报告</h1>
<div class="sub">生成时间 {{.Time}} · 节点 {{.Total}} 个，可用 {{.Alive}} 个 · 评分 = 可用性(20) + 延迟(30) + 速度(30) + IP质量(20)</div>

{{if .Top}}
<h2 style="font-size:16px;margin-bottom:10px">🏆 最优节点 Top {{len .Top}}</h2>
<div class="cards">
{{range $i, $n := .Top}}
<div class="card">
  <div class="rank">#{{add $i 1}} · {{$n.Type}}</div>
  <div class="name">{{$n.Node}}</div>
  <div class="total">{{printf "%.0f" $n.Total}}<span style="font-size:13px;color:#94a3b8"> / 100</span></div>
  <div class="meta">
    延迟 {{if $n.Ping}}{{printf "%.0f" $n.Ping.AvgMs}}ms{{else}}-{{end}} ·
    速度 {{if $n.Speed}}{{printf "%.1f" $n.Speed.DownMbps}}Mbps{{else}}-{{end}}<br>
    {{$n.Check.ExitIP}} {{if $n.IP}}{{$n.IP.Location}}{{end}}
  </div>
</div>
{{end}}
</div>
{{end}}

<h2 style="font-size:16px;margin-bottom:10px">全部节点</h2>
<table>
<tr><th>#</th><th>节点</th><th>类型</th><th>状态</th><th>总分</th><th>可用性/20</th><th>延迟/30</th><th>速度/30</th><th>IP质量/20</th><th>延迟ms</th><th>速度Mbps</th><th>出口 / 归属地</th></tr>
{{range $i, $n := .Nodes}}
<tr>
  <td>{{add $i 1}}</td>
  <td>{{$n.Node}}</td>
  <td>{{$n.Type}}</td>
  <td>{{if $n.Alive}}<span class="ok">✅</span>{{else}}<span class="bad">❌</span>{{end}}</td>
  <td><b>{{printf "%.0f" $n.Total}}</b><span class="barbg"><i style="width:{{printf "%.0f" $n.Total}}%"></i></span></td>
  <td>{{printf "%.0f" $n.ScoreAvail}}</td>
  <td>{{printf "%.0f" $n.ScoreLat}}</td>
  <td>{{printf "%.0f" $n.ScoreSpeed}}</td>
  <td>{{printf "%.0f" $n.ScoreIPQ}}</td>
  <td>{{if $n.Ping}}{{if gt $n.Ping.Recv 0}}{{printf "%.0f" $n.Ping.AvgMs}}{{else}}100%丢包{{end}}{{else}}-{{end}}</td>
  <td>{{if $n.Speed}}{{if eq $n.Speed.Err ""}}{{printf "%.1f" $n.Speed.DownMbps}}{{else}}失败{{end}}{{else}}-{{end}}</td>
  <td>{{$n.Check.ExitIP}} {{if $n.IP}}{{$n.IP.Location}}{{if $n.IP.Flags}} <span class="tag">{{$n.IP.Flags}}</span>{{end}}{{end}}</td>
</tr>
{{end}}
</table>
<footer>由 netscope 生成 · 自包含单文件，可离线分享</footer>
</div></body></html>
`))

// RenderReport 生成自包含 HTML。
func RenderReport(nodes []NodeScore, topN int) ([]byte, error) {
	if topN > len(nodes) {
		topN = len(nodes)
	}
	type page struct {
		Time  string
		Total int
		Alive int
		Top   []NodeScore
		Nodes []NodeScore
	}
	p := page{
		Time:  time.Now().Format("2006-01-02 15:04:05"),
		Total: len(nodes),
		Nodes: nodes,
	}
	for _, n := range nodes {
		if n.Alive {
			p.Alive++
		}
	}
	if topN > 0 {
		for _, n := range nodes {
			if len(p.Top) >= topN {
				break
			}
			if n.Alive {
				p.Top = append(p.Top, n)
			}
		}
	}
	var sb strings.Builder
	if err := reportTpl.Execute(&sb, p); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// SaveReport 把 HTML 报告与 JSON 快照写入报告目录，返回路径。
func SaveReport(dir string, nodes []NodeScore, topN int) (htmlPath, jsonPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := time.Now().Format("20060102-150405")
	htmlPath = filepath.Join(dir, "rate-"+stamp+".html")
	jsonPath = filepath.Join(dir, "rate-"+stamp+".json")
	body, err := RenderReport(nodes, topN)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(htmlPath, body, 0o644); err != nil {
		return "", "", err
	}
	if err := WriteJSON(jsonPath, map[string]any{
		"time":  time.Now().Format(time.RFC3339),
		"nodes": nodes,
	}); err != nil {
		return "", "", err
	}
	return htmlPath, jsonPath, nil
}

// ---------- sub rate 命令 ----------

func cmdSubRate(ctx context.Context, args []string) int {
	fs := newFlagSet("sub rate")
	sf := addSubFlags(fs, 5)
	targets := &multiFlag{}
	fs.Var(targets, "t", "可用性检测目标 URL（可重复）")
	top := fs.Int("top", 5, "Top N 推荐")
	pingN := fs.Int("n", 5, "每节点 ping 次数")
	size := fs.Int64("size", 30<<20, "单节点最大下载字节")
	durFlag := secsDur{5 * time.Second}
	fs.Var(&durFlag, "dur", "单节点最长测速时长（裸数字按秒）")
	reportDir := fs.String("report-dir", defaultReportDir(), "报告目录")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	fs.Parse(args)

	nodes := sf.load(ctx)
	opt := RateOptions{
		Targets:     *targets,
		PingCount:   *pingN,
		SpeedSize:   *size,
		SpeedDur:    durFlag.Duration,
		Timeout:     sf.timeout.Duration,
		Concurrency: *sf.conc,
	}
	done := 0
	scores := RateNodes(ctx, nodes, opt, func(ns NodeScore) {
		done++
		Progress("\r评分进度 [%d/%d]", done, len(nodes))
	})
	Progress("\n")

	headers := []string{"#", "节点", "类型", "总分", "可用", "延迟ms", "速度Mbps", "出口 / 归属地"}
	var rows [][]string
	alive := 0
	for i, ns := range scores {
		if ns.Alive {
			alive++
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", i+1), ns.Node, ns.Type,
			fmt.Sprintf("%.0f", ns.Total), yesNo(ns.Alive),
			dashIf(ns.Ping != nil && ns.Ping.Recv > 0, func() string { return f1(ns.Ping.AvgMs) }),
			dashIf(ns.Speed != nil && ns.Speed.Err == "", func() string { return f1(ns.Speed.DownMbps) }),
			ns.Check.ExitIP + " " + ns.IP.Location(),
		})
	}
	PrintTable(headers, rows)

	topN := *top
	var aliveScores []NodeScore
	for _, sc := range scores {
		if sc.Alive {
			aliveScores = append(aliveScores, sc)
		}
	}
	if topN > len(aliveScores) {
		topN = len(aliveScores)
	}
	if topN > 0 {
		fmt.Printf("\n🏆 Top %d 推荐：\n", topN)
		for i := 0; i < topN; i++ {
			fmt.Printf("  %d. %s（%.0f 分）\n", i+1, aliveScores[i].Node, aliveScores[i].Total)
		}
	}
	htmlPath, jsonPath, err := SaveReport(*reportDir, scores, topN)
	if err != nil {
		Progress("保存报告失败: %v\n", err)
	} else {
		Progress("HTML 报告: %s\nJSON 快照: %s\n", htmlPath, jsonPath)
	}
	writeJSONIfSet(*jsonOut, map[string]any{"nodes": scores})
	return boolCode(alive > 0)
}

func yesNo(b bool) string {
	if b {
		return "✅"
	}
	return "❌"
}

func dashIf(cond bool, f func() string) string {
	if cond {
		return f()
	}
	return "-"
}
