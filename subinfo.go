package main

// sub info：读取订阅的 subscription-userinfo 响应头，展示流量用量与到期时间。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// SubInfo 是一个订阅的用量信息（subscription-userinfo 头）。
type SubInfo struct {
	Source    string `json:"source"`
	NodeCount int    `json:"nodeCount"`
	Upload    int64  `json:"upload"`           // 字节
	Download  int64  `json:"download"`         // 字节
	Total     int64  `json:"total,omitempty"`  // 字节；0 表示未知
	Expire    int64  `json:"expire,omitempty"` // unix 秒；0 表示未知
	HasUsage  bool   `json:"hasUsage"`
	Err       string `json:"err,omitempty"`
}

// FetchSubInfo 拉取一个订阅源：解析节点数并读取 subscription-userinfo 头。
func FetchSubInfo(ctx context.Context, src string) SubInfo {
	info := SubInfo{Source: src}
	var data []byte
	var hdr http.Header
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		b, h, err := httpGetRetry(ctx, src, 2)
		if err != nil {
			info.Err = err.Error()
			return info
		}
		data, hdr = b, h
	} else {
		b, err := os.ReadFile(strings.TrimPrefix(src, "file://"))
		if err != nil {
			info.Err = err.Error()
			return info
		}
		data = b
	}
	if nodes, err := parseSubscriptionContent(data); err == nil {
		info.NodeCount = len(nodes)
	}
	if hdr != nil {
		parseUserInfo(hdr.Get("Subscription-Userinfo"), &info)
	}
	return info
}

// parseUserInfo 解析 `upload=123; download=456; total=789; expire=1700000000`。
func parseUserInfo(v string, info *SubInfo) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	for _, kv := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload = n
			info.HasUsage = true
		case "download":
			info.Download = n
			info.HasUsage = true
		case "total":
			info.Total = n
			info.HasUsage = true
		case "expire":
			if n > 1e12 { // 个别面板给毫秒
				n /= 1000
			}
			info.Expire = n
		}
	}
}

// fmtBytes 把字节数格式化为人类可读（GB 优先）。
func fmtBytes(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1fGB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1fMB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1fKB", float64(n)/KB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func fmtTimeUnix(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	return time.Unix(sec, 0).Format("2006-01-02")
}

// cmdSubInfo 实现 `sub info`。
func cmdSubInfo(ctx context.Context, args []string) int {
	fs := newFlagSet("sub info")
	subs := &multiFlag{}
	fs.Var(subs, "sub", "订阅链接或本地文件（可重复）")
	jsonOut := fs.String("json", "", "JSON 输出路径（- 为 stdout）")
	fs.Parse(args)
	if len(*subs) == 0 {
		fatalf("缺少 -sub 参数（订阅链接或本地文件）")
	}

	var infos []SubInfo
	for _, src := range *subs {
		Progress("读取 %s …\n", src)
		infos = append(infos, FetchSubInfo(ctx, src))
	}

	headers := []string{"订阅", "节点数", "已用", "总量", "剩余", "使用率", "到期", "剩余天数"}
	var rows [][]string
	for _, in := range infos {
		row := []string{in.Source, strconv.Itoa(in.NodeCount)}
		switch {
		case in.Err != "":
			row = append(row, "读取失败", "-", "-", "-", "-", "-")
		case !in.HasUsage:
			row = append(row, "无用量信息", "-", "-", "-", "-", "-")
		default:
			used := in.Upload + in.Download
			row = append(row, fmtBytes(used))
			if in.Total > 0 {
				pct := float64(used) / float64(in.Total) * 100
				row = append(row, fmtBytes(in.Total), fmtBytes(in.Total-used), fmt.Sprintf("%.1f%%", pct))
			} else {
				row = append(row, "不限", "-", "-")
			}
			if in.Expire > 0 {
				days := int(time.Until(time.Unix(in.Expire, 0)).Hours() / 24)
				row = append(row, fmtTimeUnix(in.Expire), fmt.Sprintf("%d 天", days))
			} else {
				row = append(row, "未知", "-")
			}
		}
		rows = append(rows, row)
	}
	PrintTable(headers, rows)
	writeJSONIfSet(*jsonOut, map[string]any{"subs": infos})
	return 0
}
