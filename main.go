package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/log"
)

const usageText = `netscope - 一站式网络体检工具

用法:
  netscope sub check   [-sub 链接/文件]... [-t URL]...     节点批量可用性（状态码/耗时/出口IP）
  netscope sub ping    [-sub ...] [-t URL] [-n 次数]       节点延迟与丢包（TCP ping，--udp 附 STUN 探测）
  netscope sub speed   [-sub ...] [--size 字节] [--dur 秒]  节点测速（Cloudflare）
  netscope sub rate    [-sub ...] [--top N]                综合评分 + Top N + HTML 报告
  netscope sub unlock  [-sub ...] [--services 名单]        流媒体/AI 解锁检测（Netflix/Disney+/ChatGPT…）
  netscope sub info    [-sub 链接]...                      订阅用量面板（流量剩余/到期）
  netscope route ping  <主机> [-n 次数]                     本机 ICMP ping（无权限降级 TCP）
  netscope route trace <主机> [-mttl 最大跳数]              UDP traceroute（免 root）
  netscope route bloat [主机] [--dur 秒] [--streams N]     bufferbloat：满载下载时的延迟变化
  netscope port probe  <主机> -p <端口>[/tcp|/udp]...      端口连通性
  netscope http inspect <URL> [--via 节点]                 证书/TLS/HTTP 版本体检
  netscope dns audit   <域名> [--via 节点]                 多 resolver 对比 + 污染检测 + EDNS 出口
  netscope ip show     [--via 节点]                        国内外出口 IP 双视角 + IP 风险分
  netscope report diff [旧.json 新.json]                   两次评分快照对比（默认取最新两个）
  netscope report clean [--keep N --keep-days D]          清理报告快照（默认读配置）
  netscope serve       [--listen 0.0.0.0:8420] [--sub 订阅] Web 体检台（局域网）
  netscope config init|show                               生成/查看 YAML 配置预设

通用: --config 路径 或 NETSCOPE_CONFIG 环境变量指定配置；--json 路径 输出 JSON（"-" 为 stdout）；
      --csv 文件；--via 支持 节点名 或 序号(1起)
所有检测均为一次性手动触发，不驻留、不定时、不自动循环。
`

func usage() {
	fmt.Fprint(os.Stderr, usageText)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(2)
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

// parseMixed 支持位置参数与 flag 混排（如 `port probe host -p 80`）。
// 返回位置参数列表。
// secsDur 是宽松的时长 flag：`5` 按秒，`500ms`/`1m30s` 走标准解析。
type secsDur struct{ time.Duration }

func (d *secsDur) String() string { return d.Duration.String() }
func (d *secsDur) Set(v string) error {
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	x, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	d.Duration = x
	return nil
}

func parseMixed(fs *flag.FlagSet, args []string) []string {
	boolFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			boolFlags[f.Name] = true
		}
	})
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(a, "=") && !boolFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	fs.Parse(flags)
	return pos
}

// globalConfigPath 指向实际使用的配置文件（config init/show 用）。
var globalConfigPath = defaultConfigPath()

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	log.SetLevel(log.SILENT)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 全局 --config（须在子命令之前）与环境变量
	args := os.Args[1:]
	if len(args) >= 2 && (args[0] == "--config" || args[0] == "-config") {
		globalConfigPath = args[1]
		args = args[2:]
	} else if strings.HasPrefix(args[0], "--config=") {
		globalConfigPath = strings.TrimPrefix(args[0], "--config=")
		args = args[1:]
	} else if v := os.Getenv("NETSCOPE_CONFIG"); v != "" {
		globalConfigPath = v
	}
	if cfg, err := loadConfig(globalConfigPath); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 配置加载失败：%v（使用默认值）\n", err)
	} else {
		appConfig = cfg
	}

	group, cmd := args[0], ""
	args = args[1:]
	switch group {
	case "sub", "route", "port", "http", "dns", "ip", "report", "config":
		if len(args) == 0 {
			usage()
			os.Exit(2)
		}
		cmd, args = args[0], args[1:]
	}

	var code int
	switch strings.TrimSpace(group + " " + cmd) {
	case "sub check":
		code = cmdSubCheck(ctx, args)
	case "sub ping":
		code = cmdSubPing(ctx, args)
	case "sub speed":
		code = cmdSubSpeed(ctx, args)
	case "sub rate":
		code = cmdSubRate(ctx, args)
	case "sub unlock":
		code = cmdSubUnlock(ctx, args)
	case "sub info":
		code = cmdSubInfo(ctx, args)
	case "route ping":
		code = cmdRoutePing(ctx, args)
	case "route trace":
		code = cmdRouteTrace(ctx, args)
	case "route bloat":
		code = cmdRouteBloat(ctx, args)
	case "port probe":
		code = cmdPortProbe(ctx, args)
	case "http inspect":
		code = cmdHTTPInspect(ctx, args)
	case "dns audit":
		code = cmdDNSAudit(ctx, args)
	case "ip show":
		code = cmdIPShow(ctx, args)
	case "report diff":
		code = cmdReportDiff(ctx, args)
	case "report clean":
		code = cmdReportClean(ctx, args)
	case "serve":
		code = cmdServe(ctx, args)
	case "config init":
		code = cmdConfigInit(args)
	case "config show":
		code = cmdConfigShow(args)
	case "version":
		fmt.Println("netscope 0.3.0 (P2)")
	default:
		usage()
		os.Exit(2)
	}
	os.Exit(code)
}

// ---------- 公共 flag ----------

type multiFlag []string

func (m *multiFlag) String() string { return "" }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type subFlags struct {
	subs    *multiFlag
	include *multiFlag
	exclude *multiFlag
	conc    *int
	timeout *secsDur
}

func addSubFlags(fs *flag.FlagSet, defaultConc int) *subFlags {
	s := &subFlags{
		subs:    &multiFlag{},
		include: &multiFlag{},
		exclude: &multiFlag{},
	}
	fs.Var(s.subs, "sub", "订阅链接或本地文件（可重复）")
	fs.Var(s.include, "include", "仅检测名称包含关键字的节点（可重复）")
	fs.Var(s.exclude, "exclude", "跳过名称包含关键字的节点（可重复）")
	s.conc = fs.Int("c", defaultConc, "并发数")
	s.timeout = &secsDur{10 * time.Second}
	fs.Var(s.timeout, "timeout", "单请求超时")
	return s
}

func (s *subFlags) load(ctx context.Context) []Tunnel {
	if len(*s.subs) == 0 {
		fatalf("缺少 -sub 参数（订阅链接或本地文件）")
	}
	nodes, err := LoadSubscriptions(ctx, *s.subs, *s.include, *s.exclude)
	if err != nil {
		fatalf("%v", err)
	}
	if len(nodes) == 0 {
		fatalf("没有匹配的节点")
	}
	Progress("共 %d 个节点待检测\n", len(nodes))
	return nodes
}

// ---------- sub 组 ----------

func cmdSubCheck(ctx context.Context, args []string) int {
	fs := newFlagSet("sub check")
	sf := addSubFlags(fs, 20)
	targets := &multiFlag{}
	fs.Var(targets, "t", "检测目标 URL（可重复，默认 gstatic 204）")
	ipinfo := fs.Bool("ipinfo", true, "检测出口 IP 与归属地")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	csvOut := fs.String("csv", "", "CSV 输出路径")
	fs.Parse(args)

	tg := *targets
	if len(tg) == 0 {
		tg = appConfig.Targets // 配置文件里的默认目标
	}
	if len(tg) == 0 {
		tg = []string{"https://www.gstatic.com/generate_204"}
	}
	nodes := sf.load(ctx)
	total := len(nodes) * len(tg)
	done := 0
	results := RunChecks(ctx, nodes, tg, sf.timeout.Duration, *sf.conc, func(r CheckResult) {
		done++
		Progress("\r[%d/%d] %s", done, total, pad(r.Node, 30))
	})
	Progress("\n")

	headers := []string{"节点", "类型", "目标", "状态", "状态码", "建连ms", "总耗时ms", "出口IP", "归属地"}
	var rows [][]string
	anyOK := false
	for _, r := range results {
		ok, code := "❌", "-"
		if r.OK {
			ok, code = "✅", strconv.Itoa(r.Status)
			anyOK = true
		}
		row := []string{r.Node, r.NodeType, shortTarget(r.Target), ok, code, numOrDash(r.ConnMs), numOrDash(r.TotalMs)}
		if *ipinfo {
			row = append(row, orDash(r.ExitIP), orDash(r.Location))
		} else {
			headers = headers[:7]
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, map[string]any{"results": results})
	writeCSVIfSet(*csvOut, headers, rows)
	return boolCode(anyOK)
}

func cmdSubPing(ctx context.Context, args []string) int {
	fs := newFlagSet("sub ping")
	sf := addSubFlags(fs, 20)
	target := fs.String("t", "https://www.gstatic.com/generate_204", "ping 目标（取其 host:port）")
	count := fs.Int("n", 5, "每节点 ping 次数")
	interval := fs.Duration("interval", 300*time.Millisecond, "ping 间隔")
	udpFlag := fs.Bool("udp", false, "附加 STUN UDP 探测（节点 UDP 能力与出口）")
	stun := fs.String("stun", "stun.l.google.com:19302", "--udp 使用的 STUN 服务器")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	csvOut := fs.String("csv", "", "CSV 输出路径")
	fs.Parse(args)

	addr, err := SplitHostPortDefault(*target)
	if err != nil {
		fatalf("%v", err)
	}
	nodes := sf.load(ctx)
	var mu sync.Mutex
	var stats []PingStats
	udpRes := map[string]UDPPingResult{}
	var tasks []func(context.Context)
	for _, n := range nodes {
		n := n
		tasks = append(tasks, func(ctx context.Context) {
			st := TCPPing(ctx, n, addr, *count, sf.timeout.Duration, *interval)
			mu.Lock()
			stats = append(stats, st)
			mu.Unlock()
			if st.Recv == 0 {
				Progress("  %s 丢包 100%%\n", n.Name())
			} else {
				Progress("  %s avg %.1fms\n", n.Name(), st.AvgMs)
			}
			if *udpFlag {
				ur := STUNPing(ctx, n, *stun, *count, sf.timeout.Duration, *interval)
				mu.Lock()
				udpRes[n.Name()] = ur
				mu.Unlock()
				if ur.OK {
					Progress("  %s UDP ✅ %s\n", n.Name(), ur.ExitAddr)
				} else {
					Progress("  %s UDP ❌ %s\n", n.Name(), ur.Err)
				}
			}
		})
	}
	RunParallel(ctx, *sf.conc, tasks)

	headers := []string{"节点", "目标", "发送", "接收", "丢包%", "min(ms)", "avg(ms)", "max(ms)", "抖动(ms)"}
	if *udpFlag {
		headers = append(headers, "UDP")
	}
	var rows [][]string
	anyOK := false
	for _, st := range stats {
		row := []string{st.Node, st.Target, strconv.Itoa(st.Sent), strconv.Itoa(st.Recv), fmt.Sprintf("%.0f", st.Loss)}
		if st.Recv > 0 {
			anyOK = true
			row = append(row, f1(st.MinMs), f1(st.AvgMs), f1(st.MaxMs), f1(st.Jitter))
		} else {
			row = append(row, "-", "-", "-", "-")
		}
		if *udpFlag {
			row = append(row, udpCell(udpRes[st.Node]))
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	out := map[string]any{"results": stats}
	if *udpFlag {
		out["udp"] = udpRes
	}
	writeJSONIfSet(*jsonOut, out)
	writeCSVIfSet(*csvOut, headers, rows)
	return boolCode(anyOK)
}

// udpCell 把 UDP 探测结果格式化为表格单元。
func udpCell(u UDPPingResult) string {
	switch {
	case !u.Supported:
		return "❌ 不支持"
	case u.OK:
		return "✅ " + u.ExitAddr
	default:
		return "❌"
	}
}

func cmdSubSpeed(ctx context.Context, args []string) int {
	fs := newFlagSet("sub speed")
	sf := addSubFlags(fs, 1) // 默认并发 1，避免节点间抢带宽
	size := fs.Int64("size", 50<<20, "单节点最大下载字节")
	durFlag := secsDur{10 * time.Second}
	fs.Var(&durFlag, "dur", "单节点最长测速时长")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	fs.Parse(args)

	nodes := sf.load(ctx)
	var mu sync.Mutex
	var results []SpeedResult
	var tasks []func(context.Context)
	for _, n := range nodes {
		n := n
		tasks = append(tasks, func(ctx context.Context) {
			r := MeasureSpeed(ctx, n, *size, durFlag.Duration)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
			if r.Err != "" {
				Progress("  %s 失败: %s\n", n.Name(), r.Err)
			} else {
				Progress("  %s ↓ %.1f Mbps\n", n.Name(), r.DownMbps)
			}
		})
	}
	RunParallel(ctx, *sf.conc, tasks)

	headers := []string{"节点", "下载Mbps", "读取MB", "用时s"}
	var rows [][]string
	anyOK := false
	for _, r := range results {
		row := []string{r.Node}
		if r.Err != "" {
			row = append(row, "失败("+shortErr(r.Err)+")", "-", "-")
		} else {
			anyOK = true
			row = append(row, f1(r.DownMbps), f1(float64(r.BytesRead)/(1<<20)), f1(r.DurationS))
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, map[string]any{"results": results})
	return boolCode(anyOK)
}

// ---------- 诊断组 ----------

func viaFlags(fs *flag.FlagSet) (via, sub *string) {
	via = fs.String("via", "", "探测通道：direct / 节点名 / 序号(1起)")
	sub = fs.String("sub", "", "订阅（--via 选择节点时需要）")
	return
}

func resolveVia(ctx context.Context, via, sub string) Tunnel {
	if via == "" || via == "direct" {
		return Direct
	}
	if sub == "" {
		fatalf("--via 需要同时指定 --sub 以加载节点")
	}
	nodes, err := LoadSubscriptions(ctx, []string{sub}, nil, nil)
	if err != nil {
		fatalf("%v", err)
	}
	t, err := BuildTunnel(nodes, via)
	if err != nil {
		fatalf("%v", err)
	}
	return t
}

func cmdRoutePing(ctx context.Context, args []string) int {
	fs := newFlagSet("route ping")
	count := fs.Int("n", 4, "次数")
	timeoutFlag := secsDur{time.Second}
	fs.Var(&timeoutFlag, "timeout", "单次超时")
	interval := fs.Duration("interval", time.Second, "间隔")
	tcpPort := fs.Int("tcp-port", 443, "降级 TCP ping 时使用的端口")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	if len(pos) < 1 {
		fatalf("用法: netscope route ping <主机>")
	}
	host := pos[0]

	lines, st, err := RoutePing(ctx, host, *count, timeoutFlag.Duration, *interval)
	if err != nil {
		Progress("ICMP 不可用（%v），降级 TCP ping %s:%d\n", err, host, *tcpPort)
		addr := net.JoinHostPort(host, strconv.Itoa(*tcpPort))
		tp := TCPPing(ctx, Direct, addr, *count, timeoutFlag.Duration, *interval)
		headers := []string{"目标", "模式", "发送", "接收", "丢包%", "min", "avg", "max", "抖动"}
		row := []string{tp.Target, "tcp", strconv.Itoa(tp.Sent), strconv.Itoa(tp.Recv), fmt.Sprintf("%.0f", tp.Loss)}
		if tp.Recv > 0 {
			row = append(row, f1(tp.MinMs), f1(tp.AvgMs), f1(tp.MaxMs), f1(tp.Jitter))
		} else {
			row = append(row, "-", "-", "-", "-")
		}
		PrintTable(headers, [][]string{row})
		writeJSONIfSet(*jsonOut, map[string]any{"mode": "tcp", "stats": tp})
		return boolCode(tp.Recv > 0)
	}
	fmt.Printf("PING %s（%s）共 %d 次\n", host, st.Target, st.Sent)
	for _, l := range lines {
		if l.Timeout {
			fmt.Printf("  seq=%-3d 超时\n", l.Seq)
		} else {
			fmt.Printf("  seq=%-3d from %s time=%.3fms\n", l.Seq, l.IP, l.RTTms)
		}
	}
	fmt.Printf("统计: 发送 %d 接收 %d 丢包 %.0f%%", st.Sent, st.Recv, st.Loss)
	if st.Recv > 0 {
		fmt.Printf("，min/avg/max/抖动 = %.1f/%.1f/%.1f/%.1f ms", st.MinMs, st.AvgMs, st.MaxMs, st.Jitter)
	}
	fmt.Println()
	writeJSONIfSet(*jsonOut, map[string]any{"mode": "icmp", "lines": lines, "stats": st})
	return boolCode(st.Recv > 0)
}

func cmdRouteTrace(ctx context.Context, args []string) int {
	fs := newFlagSet("route trace")
	maxTTL := fs.Int("mttl", 30, "最大跳数")
	probes := fs.Int("q", 3, "每跳探测次数")
	timeoutFlag := secsDur{time.Second}
	fs.Var(&timeoutFlag, "timeout", "单探测超时")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	if len(pos) < 1 {
		fatalf("用法: netscope route trace <主机>")
	}
	host := pos[0]
	Progress("traceroute 到 %s，最多 %d 跳\n", host, *maxTTL)
	hops, err := RouteTrace(ctx, host, *maxTTL, *probes, timeoutFlag.Duration)
	headers := []string{"跳", "IP", "归属地", "RTT(ms)"}
	var rows [][]string
	for _, h := range hops {
		row := []string{strconv.Itoa(h.TTL)}
		if h.Star {
			row = append(row, "*", "*", "*")
		} else {
			row = append(row, h.IP, orDash(h.Loc), f1(h.RTTms))
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	if err != nil && ctx.Err() == nil {
		Progress("警告: %v\n", err)
	}
	writeJSONIfSet(*jsonOut, map[string]any{"target": host, "hops": hops})
	return 0
}

func cmdPortProbe(ctx context.Context, args []string) int {
	fs := newFlagSet("port probe")
	ports := &multiFlag{}
	fs.Var(ports, "p", "端口，如 80 / 443/tcp / 53/udp（可重复）")
	timeoutFlag := secsDur{2 * time.Second}
	fs.Var(&timeoutFlag, "timeout", "超时")
	via, sub := viaFlags(fs)
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	if len(pos) < 1 {
		fatalf("用法: netscope port probe <主机> -p <端口>...")
	}
	if len(*ports) == 0 {
		fatalf("缺少 -p 端口参数")
	}
	t := resolveVia(ctx, *via, *sub)
	results := ProbePorts(ctx, t, pos[0], *ports, timeoutFlag.Duration)
	headers := []string{"主机", "端口", "协议", "状态", "延迟(ms)", "说明"}
	var rows [][]string
	anyOpen := false
	for _, r := range results {
		if r.State == "open" {
			anyOpen = true
		}
		rows = append(rows, []string{r.Host, strconv.Itoa(r.Port), r.Proto, r.State, numOrDash(r.LatencyMs), orDash(r.Err)})
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, map[string]any{"results": results})
	return boolCode(anyOpen)
}

func cmdHTTPInspect(ctx context.Context, args []string) int {
	fs := newFlagSet("http inspect")
	timeoutFlag := secsDur{10 * time.Second}
	fs.Var(&timeoutFlag, "timeout", "超时")
	via, sub := viaFlags(fs)
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	if len(pos) < 1 {
		fatalf("用法: netscope http inspect <URL>")
	}
	t := resolveVia(ctx, *via, *sub)
	r := InspectHTTP(ctx, t, pos[0], timeoutFlag.Duration)
	headers := []string{"项目", "值"}
	rows := [][]string{
		{"URL", r.URL},
		{"通道", r.Via},
		{"HTTP 版本", orDash(r.HTTPVersion)},
		{"状态码", numOrDash(float64(r.Status))},
		{"TLS 版本", orDash(r.TLSVersion)},
		{"加密套件", orDash(r.CipherSuite)},
		{"ALPN", orDash(r.ALPN)},
		{"证书签发者", orDash(r.CertIssuer)},
		{"证书主体", orDash(r.CertSubject)},
		{"SAN 数量", strconv.Itoa(r.CertSANs)},
		{"证书链深度", strconv.Itoa(r.ChainDepth)},
		{"剩余有效期(天)", strconv.Itoa(r.DaysLeft)},
	}
	for _, n := range r.Notes {
		rows = append(rows, []string{"⚠️", n})
	}
	if r.Err != "" {
		rows = append(rows, []string{"错误", r.Err})
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, r)
	return boolCode(r.Err == "")
}

func cmdDNSAudit(ctx context.Context, args []string) int {
	fs := newFlagSet("dns audit")
	via, sub := viaFlags(fs)
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)
	if len(pos) < 1 {
		fatalf("用法: netscope dns audit <域名>")
	}
	t := resolveVia(ctx, *via, *sub)
	res := DNSAudit(ctx, pos[0], t)
	results := res.Resolvers
	headers := []string{"Resolver", "类型", "A 记录", "TTL", "耗时(ms)", "错误"}
	var rows [][]string
	for _, r := range results {
		addr := "-"
		if len(r.Addrs) > 0 {
			addr = strings.Join(r.Addrs, " ")
			if dispWidth(addr) > 60 {
				addr = r.Addrs[0] + fmt.Sprintf(" +%d", len(r.Addrs)-1)
			}
		}
		ttl := "-"
		if r.TTL > 0 {
			ttl = strconv.FormatUint(uint64(r.TTL), 10)
		}
		rows = append(rows, []string{r.Resolver, r.Type, addr, ttl, f1(r.RttMs), orDash(r.Err)})
	}
	PrintTable(headers, rows)
	sets := map[string][]string{}
	for _, r := range results {
		if len(r.Addrs) > 0 {
			k := strings.Join(r.Addrs, ",")
			sets[k] = append(sets[k], r.Resolver)
		}
	}
	if len(sets) > 1 {
		fmt.Println("⚠️ 各 resolver 解析结果不一致：")
		for s, rs := range sets {
			fmt.Printf("  %s <- %s\n", strings.ReplaceAll(s, ",", ", "), strings.Join(rs, "、"))
		}
	}
	if p := res.Pollution; p != nil {
		icon := "ℹ️"
		if p.Polluted {
			icon = "⚠️"
		}
		fmt.Printf("\n%s 污染检测（%s）：%s\n", icon, p.Domain, p.Note)
		fmt.Printf("   明文 UDP: %s\n   加密 DoH: %s\n", orDash(strings.Join(p.UDPAddrs, " ")), orDash(strings.Join(p.DoHAddrs, " ")))
	}
	if len(res.EDNS) > 0 {
		fmt.Println("\n递归出口与 EDNS Client Subnet（o-o.myaddr.l.google.com）：")
		eh := []string{"Resolver", "递归出口", "出口归属地", "ECS"}
		var erows [][]string
		for _, e := range res.EDNS {
			egress := "-"
			if len(e.EgressIPs) > 0 {
				egress = strings.Join(e.EgressIPs, " ")
			}
			erows = append(erows, []string{e.Resolver, egress, orDash(e.EgressLoc), orDash(e.ECS)})
		}
		PrintTable(eh, erows)
	}
	writeJSONIfSet(*jsonOut, res)
	return 0
}

func cmdIPShow(ctx context.Context, args []string) int {
	fs := newFlagSet("ip show")
	via, sub := viaFlags(fs)
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	fs.Parse(args)
	t := resolveVia(ctx, *via, *sub)
	r := IPShow(ctx, t)
	headers := []string{"视角", "出口IP", "归属地", "标记", "风险分"}
	var rows [][]string
	if r.Domestic != nil {
		rows = append(rows, []string{"国内(ipip.net)", r.Domestic.IP, r.Domestic.Desc, "-", "-"})
	} else {
		rows = append(rows, []string{"国内(ipip.net)", "查询失败", r.IPInternalErr, "-", "-"})
	}
	if r.Global != nil {
		risk := "-"
		if v := r.Global.RiskScore(); v >= 0 {
			risk = strconv.Itoa(v)
		}
		rows = append(rows, []string{"国际(ip-api)", r.Global.Query, r.Global.Location(), r.Global.Flags(), risk})
	} else {
		rows = append(rows, []string{"国际(ip-api)", "查询失败", r.GlobalErr, "-", "-"})
	}
	rows = append(rows, []string{"通道", r.Via, "-", "-", "-"})
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, r)
	return boolCode(r.Domestic != nil || r.Global != nil)
}

// ---------- 小工具 ----------

func writeJSONIfSet(path string, v any) {
	if path == "" {
		return
	}
	if err := WriteJSON(path, v); err != nil {
		fatalf("写 JSON 失败: %v", err)
	}
	if path != "-" {
		Progress("JSON 已写入 %s\n", path)
	}
}

func writeCSVIfSet(path string, headers []string, rows [][]string) {
	if path == "" {
		return
	}
	if err := WriteCSV(path, headers, rows); err != nil {
		fatalf("写 CSV 失败: %v", err)
	}
	Progress("CSV 已写入 %s\n", path)
}

func boolCode(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func numOrDash(v float64) string {
	if v == 0 {
		return "-"
	}
	return f1(v)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }

func shortTarget(t string) string {
	if len(t) > 40 {
		return t[:37] + "..."
	}
	return t
}

func shortErr(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}
