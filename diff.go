package main

// report diff：对比两次 `sub rate` 的 JSON 快照（节点增删、评分/延迟/速度/出口变化）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rateSnapshot 是 rate-*.json 的结构。
type rateSnapshot struct {
	Time  string      `json:"time"`
	Nodes []NodeScore `json:"nodes"`
}

// DiffRow 是一个节点在两次快照间的变化。
type DiffRow struct {
	Node     string  `json:"node"`
	Type     string  `json:"type,omitempty"`
	Change   string  `json:"change"` // added / removed / kept
	OldAlive bool    `json:"oldAlive"`
	NewAlive bool    `json:"newAlive"`
	OldTotal float64 `json:"oldTotal,omitempty"`
	NewTotal float64 `json:"newTotal,omitempty"`
	Delta    float64 `json:"delta"`
	OldAvgMs float64 `json:"oldAvgMs,omitempty"`
	NewAvgMs float64 `json:"newAvgMs,omitempty"`
	OldDown  float64 `json:"oldDownMbps,omitempty"`
	NewDown  float64 `json:"newDownMbps,omitempty"`
	OldExit  string  `json:"oldExit,omitempty"`
	NewExit  string  `json:"newExit,omitempty"`
}

// loadSnapshot 读取一个快照文件。
func loadSnapshot(path string) (rateSnapshot, error) {
	var s rateSnapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if len(s.Nodes) == 0 {
		return s, fmt.Errorf("%s: 快照中没有节点数据", path)
	}
	return s, nil
}

// listSnapshots 列出报告目录里的 rate-*.json（按文件名时间戳从新到旧）。
func listSnapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "rate-") && strings.HasSuffix(e.Name(), ".json") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	return paths, nil
}

// latestSnapshots 取报告目录里最新的 n 个快照。
func latestSnapshots(dir string, n int) ([]string, error) {
	paths, err := listSnapshots(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) < n {
		return nil, fmt.Errorf("报告目录 %s 里只有 %d 个快照，不足 %d 个", dir, len(paths), n)
	}
	return paths[:n], nil
}

// DiffSnapshots 对比新旧快照，返回按「新增 -> 保留（按 Δ总分升序）-> 移除」排序的行。
func DiffSnapshots(oldSnap, newSnap rateSnapshot) []DiffRow {
	idx := map[string]NodeScore{}
	for _, n := range oldSnap.Nodes {
		idx[n.Node] = n
	}
	seen := map[string]bool{}
	var rows []DiffRow
	add := func(n NodeScore, change string) {
		r := DiffRow{Node: n.Node, Type: n.Type, Change: change}
		if change == "removed" {
			r.OldAlive, r.OldTotal = n.Alive, n.Total
			r.OldAvgMs = pingAvg(n)
			r.OldDown = speedDown(n)
			r.OldExit = n.Check.ExitIP
		} else {
			r.NewAlive, r.NewTotal = n.Alive, n.Total
			r.NewAvgMs = pingAvg(n)
			r.NewDown = speedDown(n)
			r.NewExit = n.Check.ExitIP
		}
		rows = append(rows, r)
	}
	for _, n := range newSnap.Nodes {
		seen[n.Node] = true
		old, ok := idx[n.Node]
		if !ok {
			add(n, "added")
			continue
		}
		rows = append(rows, DiffRow{
			Node: n.Node, Type: n.Type, Change: "kept",
			OldAlive: old.Alive, NewAlive: n.Alive,
			OldTotal: old.Total, NewTotal: n.Total, Delta: n.Total - old.Total,
			OldAvgMs: pingAvg(old), NewAvgMs: pingAvg(n),
			OldDown: speedDown(old), NewDown: speedDown(n),
			OldExit: old.Check.ExitIP, NewExit: n.Check.ExitIP,
		})
	}
	for _, n := range oldSnap.Nodes {
		if !seen[n.Node] {
			add(n, "removed")
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		rank := map[string]int{"added": 0, "kept": 1, "removed": 2}
		if rank[rows[i].Change] != rank[rows[j].Change] {
			return rank[rows[i].Change] < rank[rows[j].Change]
		}
		if rows[i].Change == "kept" {
			return rows[i].Delta < rows[j].Delta // 退步最多的排前面
		}
		return rows[i].Node < rows[j].Node
	})
	return rows
}

func pingAvg(n NodeScore) float64 {
	if n.Ping != nil && n.Ping.Recv > 0 {
		return n.Ping.AvgMs
	}
	return 0
}

func speedDown(n NodeScore) float64 {
	if n.Speed != nil && n.Speed.Err == "" {
		return n.Speed.DownMbps
	}
	return 0
}

func changeLabel(c string) string {
	switch c {
	case "added":
		return "🆕 新增"
	case "removed":
		return "⛔ 移除"
	default:
		return "  保留"
	}
}

// cmdReportDiff 实现 `report diff`。
func cmdReportDiff(ctx context.Context, args []string) int {
	fs := newFlagSet("report diff")
	reportDir := fs.String("report-dir", defaultReportDir(), "报告目录（无参数时取最新两个快照）")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	pos := parseMixed(fs, args)

	var oldPath, newPath string
	if len(pos) >= 2 {
		oldPath, newPath = pos[0], pos[1]
	} else {
		if len(pos) == 1 {
			fatalf("需要两个快照文件（旧 新），或都不传以自动取最新两个；收到 1 个参数: %s", pos[0])
		}
		pair, err := latestSnapshots(*reportDir, 2)
		if err != nil {
			fatalf("未指定两个快照文件，且 %v", err)
		}
		// 文件名倒序：最新的是新快照
		newPath, oldPath = pair[0], pair[1]
	}
	oldSnap, err := loadSnapshot(oldPath)
	if err != nil {
		fatalf("%v", err)
	}
	newSnap, err := loadSnapshot(newPath)
	if err != nil {
		fatalf("%v", err)
	}
	Progress("对比：\n  旧 %s（%s）\n  新 %s（%s）\n", oldPath, orDash(oldSnap.Time), newPath, orDash(newSnap.Time))

	rowsData := DiffSnapshots(oldSnap, newSnap)
	headers := []string{"变化", "节点", "旧分", "新分", "Δ分", "旧延迟ms", "新延迟ms", "旧速度Mbps", "新速度Mbps", "旧出口", "新出口"}
	var rows [][]string
	var added, removed, kept int
	var oldSum, newSum float64
	for _, d := range rowsData {
		switch d.Change {
		case "added":
			added++
		case "removed":
			removed++
		default:
			kept++
			oldSum += d.OldTotal
			newSum += d.NewTotal
		}
		rows = append(rows, []string{
			changeLabel(d.Change), d.Node,
			scoreStr(d.OldTotal, d.Change != "added"), scoreStr(d.NewTotal, d.Change != "removed"),
			dashIf(d.Change == "kept", func() string { return fmt.Sprintf("%+.0f", d.Delta) }),
			dashIf(d.OldAvgMs > 0, func() string { return f1(d.OldAvgMs) }),
			dashIf(d.NewAvgMs > 0, func() string { return f1(d.NewAvgMs) }),
			dashIf(d.OldDown > 0, func() string { return f1(d.OldDown) }),
			dashIf(d.NewDown > 0, func() string { return f1(d.NewDown) }),
			orDash(d.OldExit), orDash(d.NewExit),
		})
	}
	PrintTable(headers, rows)
	if kept > 0 {
		fmt.Printf("\n汇总：新增 %d、移除 %d、保留 %d；保留节点平均分 %.0f -> %.0f（%+.0f）\n",
			added, removed, kept, oldSum/float64(kept), newSum/float64(kept), (newSum-oldSum)/float64(kept))
	} else {
		fmt.Printf("\n汇总：新增 %d、移除 %d、保留 0\n", added, removed)
	}
	writeJSONIfSet(*jsonOut, map[string]any{
		"old":  map[string]any{"path": oldPath, "time": oldSnap.Time},
		"new":  map[string]any{"path": newPath, "time": newSnap.Time},
		"rows": rowsData,
	})
	return 0
}

func scoreStr(v float64, show bool) string {
	if !show {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}
