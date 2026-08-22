package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 目标清单持久化 ----------

type Target struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Note    string    `json:"note,omitempty"`
	Group   string    `json:"group,omitempty"`
	Created time.Time `json:"created"`
}

type targetStore struct {
	path string
	mu   sync.Mutex
	list []Target
}

func loadTargets(path string) (*targetStore, error) {
	ts := &targetStore{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &ts.list)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return ts, nil
}

func (t *targetStore) saveLocked() error {
	b, err := json.MarshalIndent(t.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(t.path, b, 0o644)
}

func (t *targetStore) All() []Target {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Target, len(t.list))
	copy(out, t.list)
	return out
}

// Groups 返回清单中出现过的分组（去重，按名称排序）。
func (t *targetStore) Groups() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, x := range t.list {
		if x.Group != "" && !seen[x.Group] {
			seen[x.Group] = true
			out = append(out, x.Group)
		}
	}
	sort.Strings(out)
	return out
}

func (t *targetStore) Add(urlStr, note, group string) (Target, error) {
	if !strings.Contains(urlStr, "://") {
		urlStr = "https://" + urlStr
	}
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		return Target{}, fmt.Errorf("仅支持 http/https 地址")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, x := range t.list {
		if x.URL == urlStr {
			return x, nil
		}
	}
	tg := Target{ID: randID(), URL: urlStr, Note: note, Group: group, Created: time.Now()}
	t.list = append(t.list, tg)
	return tg, t.saveLocked()
}

// Update 修改目标的备注与分组。
func (t *targetStore) Update(id, note, group string) (Target, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.list {
		if x.ID == id {
			t.list[i].Note = note
			t.list[i].Group = group
			_ = t.saveLocked()
			return t.list[i], true
		}
	}
	return Target{}, false
}

// Import 批量导入（按 URL 去重），返回新增数量。
func (t *targetStore) Import(items []Target) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	for _, x := range t.list {
		seen[x.URL] = true
	}
	added := 0
	for _, it := range items {
		if it.URL == "" || seen[it.URL] {
			continue
		}
		seen[it.URL] = true
		tg := Target{ID: randID(), URL: it.URL, Note: it.Note, Group: it.Group, Created: time.Now()}
		t.list = append(t.list, tg)
		added++
	}
	if added > 0 {
		_ = t.saveLocked()
	}
	return added
}

func (t *targetStore) Remove(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.list {
		if x.ID == id {
			t.list = append(t.list[:i], t.list[i+1:]...)
			_ = t.saveLocked()
			return true
		}
	}
	return false
}

func (t *targetStore) ByIDs(ids []string) []Target {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []Target
	for _, x := range t.All() {
		if want[x.ID] {
			out = append(out, x)
		}
	}
	return out
}

func randID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- 检测任务（全部由网页按钮手动触发，一次性完成） ----------

type jobItem struct {
	URL    string       `json:"url"`
	Node   string       `json:"node,omitempty"` // 全节点对比时记录所用节点
	Result *CheckResult `json:"result,omitempty"`
}

type checkJob struct {
	ID       string    `json:"id"`
	Via      string    `json:"via"`
	Total    int       `json:"total"`
	Done     int       `json:"done"`
	Finished bool      `json:"finished"`
	Started  time.Time `json:"started"`
	Items    []jobItem `json:"items"`
}

type jobManager struct {
	mu   sync.Mutex
	jobs map[string]*checkJob
}

func newJobManager() *jobManager { return &jobManager{jobs: map[string]*checkJob{}} }

// jobTask 是一个「目标 × 通道」组合。
type jobTask struct {
	URL    string
	Tunnel Tunnel
}

// Start 为 tasks 发起异步检测（conc 路并发），返回任务 ID。
func (jm *jobManager) Start(ctx context.Context, label string, tasks []jobTask, timeout time.Duration, conc int) string {
	id := randID()
	j := &checkJob{ID: id, Via: label, Started: time.Now()}
	// 只有任务涉及多个通道（全节点对比）才逐条记录节点；
	// 单通道任务每行节点相同，Via 标签已说明，避免结果表多一列冗余。
	distinct := map[string]bool{}
	for _, t := range tasks {
		distinct[t.Tunnel.Name()] = true
	}
	withNode := len(distinct) > 1
	for _, t := range tasks {
		item := jobItem{URL: t.URL}
		if withNode {
			item.Node = t.Tunnel.Name()
		}
		j.Items = append(j.Items, item)
	}
	j.Total = len(j.Items)
	jm.mu.Lock()
	jm.jobs[id] = j
	jm.mu.Unlock()
	go func() {
		var wg sync.WaitGroup
		sem := make(chan struct{}, max(conc, 1))
		for i := range j.Items {
			i := i
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					if r := recover(); r != nil {
						Progress("[warn] serve 检测任务 panic 已恢复: %v\n", r)
					}
				}()
				if ctx.Err() != nil {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, timeout)
				r := HTTPCheck(cctx, tasks[i].Tunnel, j.Items[i].URL, timeout)
				FillExitIP(cctx, tasks[i].Tunnel, &r)
				cancel()
				jm.mu.Lock()
				j.Items[i].Result = &r
				j.Done++
				jm.mu.Unlock()
			}()
		}
		wg.Wait()
		jm.mu.Lock()
		j.Finished = true
		jm.mu.Unlock()
	}()
	return id
}

func (jm *jobManager) Get(id string) *checkJob {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if j, ok := jm.jobs[id]; ok {
		cp := *j
		cp.Items = append([]jobItem{}, j.Items...)
		return &cp
	}
	return nil
}

// ---------- Web 服务 ----------

type serveDeps struct {
	targets   *targetStore
	subs      *subStore
	jobs      *jobManager
	reportDir string
	token     string
	timeout   time.Duration
	conc      int
	nodes     func() []Tunnel
	reload    func() // 订阅变更后触发重新加载
}

func buildMux(d serveDeps) *http.ServeMux {
	if d.subs == nil {
		d.subs = &subStore{} // 未提供订阅清单时兜底(仅测试场景)
	}
	if d.nodes == nil {
		d.nodes = func() []Tunnel { return nil }
	}
	mux := http.NewServeMux()

	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d.token != "" {
				t := r.URL.Query().Get("token")
				if t == "" {
					t = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				if t != d.token {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(v)
	}

	mux.Handle("/", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})))

	mux.Handle("/static/", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		var body []byte
		var ctype string
		switch name {
		case "app.js":
			body, ctype = appJS, "text/javascript; charset=utf-8"
		case "style.css":
			body, ctype = styleCSS, "text/css; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(body)
	})))

	mux.Handle("/api/state", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeList := make([]map[string]string, 0)
		for _, n := range d.nodes() {
			nodeList = append(nodeList, map[string]string{"name": n.Name(), "type": n.Type(), "server": n.Server()})
		}
		reports, _ := listReports(d.reportDir)
		writeJSON(w, map[string]any{
			"targets": d.targets.All(),
			"subs":    d.subs.All(),
			"groups":  d.targets.Groups(),
			"nodes":   nodeList,
			"reports": reports,
		})
	})))

	subsHandler := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			var req struct{ URL, Note string }
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sub, added, err := d.subs.Add(strings.TrimSpace(req.URL), req.Note)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if added && d.reload != nil {
				go d.reload()
			}
			writeJSON(w, map[string]any{"sub": sub, "added": added})
		case "DELETE":
			id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/subs"), "/")
			if id == "" || !d.subs.Remove(id) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if d.reload != nil {
				go d.reload()
			}
			writeJSON(w, map[string]bool{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	mux.Handle("/api/subs", subsHandler)
	mux.Handle("/api/subs/", subsHandler)

	targetsHandler := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/targets"), "/")
		switch {
		case r.Method == "POST" && suffix == "":
			var req struct{ URL, Note, Group string }
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || req.URL == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			tg, err := d.targets.Add(req.URL, req.Note, req.Group)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, tg)
		case r.Method == "POST" && suffix == "update":
			var req struct{ ID, Note, Group string }
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if tg, ok := d.targets.Update(req.ID, req.Note, req.Group); ok {
				writeJSON(w, tg)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
		case r.Method == "POST" && suffix == "import":
			var items []Target
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&items); err != nil {
				http.Error(w, "导入内容需为 JSON 数组", http.StatusBadRequest)
				return
			}
			added := d.targets.Import(items)
			writeJSON(w, map[string]any{"added": added})
		case r.Method == "GET" && suffix == "export":
			w.Header().Set("Content-Disposition", "attachment; filename=netscope-targets.json")
			writeJSON(w, d.targets.All())
		case r.Method == "DELETE" && suffix != "":
			if !d.targets.Remove(suffix) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	mux.Handle("/api/targets", targetsHandler)
	mux.Handle("/api/targets/", targetsHandler)

	mux.Handle("/api/jobs", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IDs []string `json:"ids"`
			Via string   `json:"via"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tg := d.targets.ByIDs(req.IDs)
		if len(tg) == 0 {
			http.Error(w, "未选择地址", http.StatusBadRequest)
			return
		}
		var tasks []jobTask
		label := req.Via
		if req.Via == "all" {
			// 全节点对比：每个地址 × 每个节点
			nodes := d.nodes()
			if len(nodes) == 0 {
				http.Error(w, "未加载订阅节点（serve 需要 --sub）", http.StatusBadRequest)
				return
			}
			if len(nodes)*len(tg) > 600 {
				http.Error(w, fmt.Sprintf("组合过多（%d 地址 × %d 节点），请减少选择", len(tg), len(nodes)), http.StatusBadRequest)
				return
			}
			for _, t := range tg {
				for _, n := range nodes {
					tasks = append(tasks, jobTask{URL: t.URL, Tunnel: n})
				}
			}
			label = fmt.Sprintf("全部节点（%d 节点）", len(nodes))
		} else {
			var t Tunnel = Direct
			if req.Via != "" && req.Via != "direct" {
				built, err := BuildTunnel(d.nodes(), req.Via)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				t = built
			}
			for _, x := range tg {
				tasks = append(tasks, jobTask{URL: x.URL, Tunnel: t})
			}
			if label == "" {
				label = "direct"
			}
		}
		id := d.jobs.Start(context.Background(), label, tasks, d.timeout, d.conc)
		writeJSON(w, map[string]string{"id": id})
	})))

	mux.Handle("/api/jobs/", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		j := d.jobs.Get(id)
		if j == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, j)
	})))

	// /api/trend：汇总报告目录里的 rate-*.json 快照，供前端画趋势图
	mux.Handle("/api/trend", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, buildTrend(d.reportDir))
	})))

	mux.Handle("/reports/", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if name == "" || strings.Contains(name, "..") || !strings.HasSuffix(name, ".html") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(d.reportDir, name))
	})))

	return mux
}

func cmdServe(ctx context.Context, args []string) int {
	fs := newFlagSet("serve")
	listen := fs.String("listen", orDefault(appConfig.Serve.Listen, "0.0.0.0:8420"), "监听地址")
	sub := fs.String("sub", "", "可选：订阅（写入订阅清单，网页上可管理多个）")
	token := fs.String("token", appConfig.Serve.Token, "可选：访问令牌（为空则不鉴权）")
	reportDir := fs.String("report-dir", orDefault(appConfig.Serve.ReportDir, defaultReportDir()), "报告目录")
	targetsPath := fs.String("targets", orDefault(appConfig.Serve.TargetsFile, defaultTargetsPath()), "地址清单存储路径")
	conc := fs.Int("c", 8, "检测并发数")
	timeoutFlag := secsDur{12 * time.Second}
	fs.Var(&timeoutFlag, "timeout", "单目标检测超时")
	fs.Parse(args)

	targets, err := loadTargets(*targetsPath)
	if err != nil {
		fatalf("读取地址清单失败: %v", err)
	}
	subs, err := loadSubs(orDefault(appConfig.Serve.SubsFile, defaultSubsPath()))
	if err != nil {
		fatalf("读取订阅清单失败: %v", err)
	}
	_ = os.MkdirAll(*reportDir, 0o755)

	// --sub 作为初始订阅写入清单（按 URL 去重）
	if *sub != "" {
		if _, added, err := subs.Add(*sub, ""); err != nil {
			Progress("订阅写入失败: %v\n", err)
		} else if added {
			Progress("已添加订阅：%s\n", *sub)
		}
	}

	loader := newNodeLoader(subs)
	go loader.Reload(ctx)

	mux := buildMux(serveDeps{
		targets:   targets,
		subs:      subs,
		jobs:      newJobManager(),
		reportDir: *reportDir,
		token:     *token,
		timeout:   timeoutFlag.Duration,
		conc:      *conc,
		nodes:     loader.Get,
		reload:    func() { loader.Reload(ctx) },
	})

	srv := &http.Server{Addr: *listen, Handler: mux}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fatalf("监听失败: %v", err)
	}
	fmt.Printf("🌐 netscope Web 体检台已启动\n")
	for _, u := range listenURLs(ln, *token) {
		fmt.Printf("   %s\n", u)
	}
	fmt.Printf("   报告目录: %s\n   地址清单: %s\n   订阅清单: %s（共 %d 条）\n   Ctrl-C 停止\n", *reportDir, *targetsPath, subs.path, len(subs.All()))
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatalf("服务异常退出: %v", err)
	}
	return 0
}

func listenURLs(ln net.Listener, token string) []string {
	port := "8420"
	if _, p, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		port = p
	}
	suffix := ""
	if token != "" {
		suffix = "?token=" + token
	}
	var out []string
	if ips, err := localIPs(); err == nil {
		for _, ip := range ips {
			out = append(out, fmt.Sprintf("http://%s:%s/%s", ip, port, suffix))
		}
	}
	return append(out, fmt.Sprintf("http://127.0.0.1:%s/%s", port, suffix))
}

func localIPs() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				out = append(out, ipnet.IP.String())
			}
		}
	}
	return out, nil
}

func listReports(dir string) ([]map[string]any, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".html") && !strings.HasSuffix(e.Name(), ".json")) {
			continue
		}
		info, _ := e.Info()
		out = append(out, map[string]any{
			"name":  e.Name(),
			"size":  info.Size(),
			"mtime": info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["mtime"].(string) > out[j]["mtime"].(string) })
	return out, nil
}

// trendNode 是一个节点在单次快照中的关键指标。
type trendNode struct {
	Node     string  `json:"node"`
	Total    float64 `json:"total"`
	Alive    bool    `json:"alive"`
	AvgMs    float64 `json:"avgMs,omitempty"`
	DownMbps float64 `json:"downMbps,omitempty"`
}

// trendSnapshot 是一次快照的全部节点指标。
type trendSnapshot struct {
	Time  string      `json:"time"` // 文件名（含时间戳）
	Label string      `json:"label"`
	Nodes []trendNode `json:"nodes"`
}

// buildTrend 按时间顺序汇总报告目录里的 rate-*.json。
func buildTrend(dir string) []trendSnapshot {
	paths, err := listSnapshots(dir)
	if err != nil {
		return nil
	}
	var out []trendSnapshot
	// listSnapshots 返回新->旧，趋势图需要旧->新
	for i := len(paths) - 1; i >= 0; i-- {
		s, err := loadSnapshot(paths[i])
		if err != nil {
			continue
		}
		ts := trendSnapshot{Time: filepath.Base(paths[i]), Label: s.Time}
		if ts.Label == "" {
			ts.Label = ts.Time
		}
		for _, n := range s.Nodes {
			tn := trendNode{Node: n.Node, Total: n.Total, Alive: n.Alive}
			if n.Ping != nil && n.Ping.Recv > 0 {
				tn.AvgMs = n.Ping.AvgMs
			}
			if n.Speed != nil && n.Speed.Err == "" {
				tn.DownMbps = n.Speed.DownMbps
			}
			ts.Nodes = append(ts.Nodes, tn)
		}
		out = append(out, ts)
	}
	return out
}

func defaultTargetsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".netscope", "targets.json")
}
