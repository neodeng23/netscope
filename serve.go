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

func (t *targetStore) Add(urlStr, note string) (Target, error) {
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
	tg := Target{ID: randID(), URL: urlStr, Note: note, Created: time.Now()}
	t.list = append(t.list, tg)
	return tg, t.saveLocked()
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

// ---------- 检测任务 ----------

type jobItem struct {
	URL    string       `json:"url"`
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

// Start 为 targets 通过 tunnel 发起异步检测（conc 路并发）。
func (jm *jobManager) Start(ctx context.Context, targets []Target, t Tunnel, timeout time.Duration, conc int) string {
	id := randID()
	j := &checkJob{ID: id, Via: t.Name(), Started: time.Now()}
	for _, tg := range targets {
		j.Items = append(j.Items, jobItem{URL: tg.URL})
	}
	j.Total = len(j.Items)
	jm.mu.Lock()
	jm.jobs[id] = j
	jm.mu.Unlock()
	go func() {
		var tasks []func(context.Context)
		for i := range j.Items {
			i := i
			tasks = append(tasks, func(ctx context.Context) {
				if ctx.Err() != nil {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, timeout)
				r := HTTPCheck(cctx, t, j.Items[i].URL, timeout)
				FillExitIP(cctx, t, &r)
				cancel()
				jm.mu.Lock()
				j.Items[i].Result = &r
				j.Done++
				jm.mu.Unlock()
			})
		}
		RunParallel(ctx, conc, tasks)
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
	jobs      *jobManager
	reportDir string
	token     string
	timeout   time.Duration
	conc      int
	nodes     func() []Tunnel
}

func buildMux(d serveDeps) *http.ServeMux {
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
			"nodes":   nodeList,
			"reports": reports,
		})
	})))

	targetsHandler := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			var req struct{ URL, Note string }
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || req.URL == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			tg, err := d.targets.Add(req.URL, req.Note)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, tg)
		case "DELETE":
			id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/targets"), "/")
			if id == "" || !d.targets.Remove(id) {
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
		var t Tunnel = Direct
		if req.Via != "" && req.Via != "direct" {
			built, err := BuildTunnel(d.nodes(), req.Via)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			t = built
		}
		id := d.jobs.Start(context.Background(), tg, t, d.timeout, d.conc)
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
	listen := fs.String("listen", "0.0.0.0:8420", "监听地址")
	sub := fs.String("sub", "", "可选：订阅，加载后网页上可选择经节点检测")
	token := fs.String("token", "", "可选：访问令牌（为空则不鉴权）")
	reportDir := fs.String("report-dir", defaultReportDir(), "报告目录")
	targetsPath := fs.String("targets", defaultTargetsPath(), "地址清单存储路径")
	conc := fs.Int("c", 8, "检测并发数")
	timeoutFlag := secsDur{12 * time.Second}
	fs.Var(&timeoutFlag, "timeout", "单目标检测超时")
	fs.Parse(args)

	targets, err := loadTargets(*targetsPath)
	if err != nil {
		fatalf("读取地址清单失败: %v", err)
	}
	_ = os.MkdirAll(*reportDir, 0o755)

	// 可选加载节点（异步，不阻塞启动）
	var nodesMu sync.Mutex
	var nodes []Tunnel
	loadNodes := func() []Tunnel {
		nodesMu.Lock()
		defer nodesMu.Unlock()
		return nodes
	}
	if *sub != "" {
		go func() {
			ns, err := LoadTunnelsFromFile(ctx, *sub)
			nodesMu.Lock()
			if err != nil {
				Progress("订阅加载失败: %v（仅可用直连检测）\n", err)
			} else {
				nodes = ns
				Progress("订阅已加载：%d 个节点可选用\n", len(ns))
			}
			nodesMu.Unlock()
		}()
	}

	mux := buildMux(serveDeps{
		targets:   targets,
		jobs:      newJobManager(),
		reportDir: *reportDir,
		token:     *token,
		timeout:   timeoutFlag.Duration,
		conc:      *conc,
		nodes:     loadNodes,
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
	fmt.Printf("   报告目录: %s\n   地址清单: %s\n   Ctrl-C 停止\n", *reportDir, *targetsPath)
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

func defaultTargetsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".netscope", "targets.json")
}
