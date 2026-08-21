package main

// 报告快照清理：rate-YYYYMMDD-HHMMSS.{html,json} 成对视为一份，
// 按保留份数（keep）与保留天数（keepDays）清理，0 表示不限。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var reportStampRe = regexp.MustCompile(`^rate-(\d{8}-\d{6})\.(html|json)$`)

// cleanReports 按策略清理报告目录，返回被删除的文件列表。
func cleanReports(dir string, keep, keepDays int) ([]string, error) {
	if keep <= 0 && keepDays <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// 按时间戳分组
	groups := map[string][]string{}
	var stamps []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := reportStampRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		stamp := m[1]
		if _, ok := groups[stamp]; !ok {
			stamps = append(stamps, stamp)
		}
		groups[stamp] = append(groups[stamp], filepath.Join(dir, e.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(stamps))) // 新→旧

	var removed []string
	now := time.Now()
	for i, stamp := range stamps {
		reason := ""
		if keep > 0 && i >= keep {
			reason = "超出保留份数"
		}
		if keepDays > 0 {
			if t, err := time.ParseInLocation("20060102-150405", stamp, now.Location()); err == nil && now.Sub(t) > time.Duration(keepDays)*24*time.Hour {
				reason = "超出保留天数"
			}
		}
		if reason == "" {
			continue
		}
		for _, f := range groups[stamp] {
			if err := os.Remove(f); err == nil {
				removed = append(removed, filepath.Base(f)+" ("+reason+")")
			}
		}
	}
	return removed, nil
}

// cmdReportClean 实现 `report clean`（也作为 sub rate 保存后的自动钩子的手动入口）。
func cmdReportClean(_ context.Context, args []string) int {
	fs := newFlagSet("report clean")
	dir := fs.String("dir", defaultReportDir(), "报告目录")
	keep := fs.Int("keep", appConfig.Reports.Keep, "保留份数（html+json 成对算一份，0 不限）")
	keepDays := fs.Int("keep-days", appConfig.Reports.KeepDays, "保留天数（0 不限）")
	fs.Parse(args)

	if *keep <= 0 && *keepDays <= 0 {
		fmt.Println("未配置清理策略（--keep/--keep-days 均为 0），不执行清理。")
		fmt.Println("可在配置文件 reports.keep / reports.keepDays 里设置，或 netscope config init 生成模板。")
		return 0
	}
	entries, _ := os.ReadDir(*dir)
	total := 0
	for _, e := range entries {
		if reportStampRe.MatchString(e.Name()) {
			total++
		}
	}
	removed, err := cleanReports(*dir, *keep, *keepDays)
	if err != nil {
		fatalf("清理失败: %v", err)
	}
	fmt.Printf("报告目录 %s：%d 个文件（%d 份）\n", *dir, total, (total+1)/2)
	if len(removed) == 0 {
		fmt.Println("无需清理。")
	} else {
		fmt.Printf("已删除 %d 个文件：\n", len(removed))
		for _, r := range removed {
			fmt.Println("  " + r)
		}
	}
	return 0
}
