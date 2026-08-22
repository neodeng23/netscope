package main

// 订阅清单持久化与节点加载：serve 网页上可添加/删除多个订阅，
// 自动拉取解析、跨订阅去重，节点进入检测通道下拉。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sub 是一条订阅（http(s) 链接或本地 Clash YAML / v2ray 文件路径）。
type Sub struct {
	ID    string    `json:"id"`
	URL   string    `json:"url"`
	Note  string    `json:"note,omitempty"`
	Added time.Time `json:"added"`
	Nodes int       `json:"nodes"`         // 最近一次加载到的节点数
	Err   string    `json:"err,omitempty"` // 最近一次加载错误
}

type subStore struct {
	path string
	mu   sync.Mutex
	list []Sub
}

func loadSubs(path string) (*subStore, error) {
	ss := &subStore{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &ss.list)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return ss, nil
}

func (s *subStore) saveLocked() error {
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

func (s *subStore) All() []Sub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sub, len(s.list))
	copy(out, s.list)
	return out
}

// Add 按 URL 去重新增订阅；已存在时返回 (原条目, false, nil)。
func (s *subStore) Add(urlStr, note string) (Sub, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.list {
		if x.URL == urlStr {
			return x, false, nil
		}
	}
	sub := Sub{ID: randID(), URL: urlStr, Note: note, Added: time.Now()}
	s.list = append(s.list, sub)
	return sub, true, s.saveLocked()
}

func (s *subStore) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.list {
		if x.ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// SetStatus 回写最近一次加载结果。
func (s *subStore) SetStatus(id string, nodes int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			s.list[i].Nodes = nodes
			s.list[i].Err = errMsg
			return
		}
	}
}

func defaultSubsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".netscope", "subs.json")
}

// ---------- 节点加载器 ----------

// nodeLoader 汇总全部订阅的节点（跨订阅按 类型+server 去重）。
type nodeLoader struct {
	mu    sync.Mutex
	nodes []Tunnel
	subs  *subStore
}

func newNodeLoader(subs *subStore) *nodeLoader {
	return &nodeLoader{subs: subs}
}

// Get 返回当前可用节点快照。
func (l *nodeLoader) Get() []Tunnel {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Tunnel, len(l.nodes))
	copy(out, l.nodes)
	return out
}

// Reload 串行拉取全部订阅并重建节点列表（本地文件秒回，远端订阅每个最长约 1 分钟超时）。
func (l *nodeLoader) Reload(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	seen := map[string]bool{}
	var merged []Tunnel
	for _, sub := range l.subs.All() {
		ns, err := LoadTunnelsFromFile(lctx, sub.URL)
		if err != nil {
			l.subs.SetStatus(sub.ID, 0, cleanErr(err))
			Progress("订阅 %s 加载失败: %v\n", sub.URL, err)
			continue
		}
		l.subs.SetStatus(sub.ID, len(ns), "")
		for _, n := range ns {
			key := n.Type() + "|" + n.Server()
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, n)
		}
		Progress("订阅 %s：加载 %d 个节点\n", sub.URL, len(ns))
	}
	l.mu.Lock()
	l.nodes = merged
	l.mu.Unlock()
}
