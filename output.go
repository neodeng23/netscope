package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"unicode"
)

// ---------- 终端表格（中文对齐） ----------

func runeWidth(r rune) int {
	if unicode.Is(unicode.Han, r) {
		return 2
	}
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、康熙、注音等
		r >= 0xAC00 && r <= 0xD7A3, // Hangul 音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意
		r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func pad(s string, width int) string {
	d := width - dispWidth(s)
	if d < 0 {
		d = 0
	}
	return s + spaces(d)
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// PrintTable 打印对齐的终端表格。rows 为字符串切片的行。
func PrintTable(headers []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Println("（无数据）")
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = dispWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && dispWidth(cell) > widths[i] {
				widths[i] = dispWidth(cell)
			}
		}
	}
	line := "+"
	for _, w := range widths {
		line += strings_Repeat("-", w+2) + "+"
	}
	fmt.Println(line)
	fmt.Print("|")
	for i, h := range headers {
		fmt.Print(" " + pad(h, widths[i]) + " |")
	}
	fmt.Println()
	fmt.Println(line)
	for _, row := range rows {
		fmt.Print("|")
		for i := range headers {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			fmt.Print(" " + pad(cell, widths[i]) + " |")
		}
		fmt.Println()
	}
	fmt.Println(line)
}

func strings_Repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// ---------- JSON / CSV 输出 ----------

// WriteJSON 输出 JSON；path 为 "-" 时打到 stdout。
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if path == "-" {
		fmt.Println(string(b))
		return nil
	}
	return os.WriteFile(path, b, 0o644)
}

// WriteCSV 输出 CSV；path 为 "-" 时打到 stdout。
func WriteCSV(path string, headers []string, rows [][]string) error {
	var f *os.File
	if path == "-" {
		f = os.Stdout
	} else {
		var err error
		f, err = os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
	}
	w := csv.NewWriter(f)
	if err := w.Write(headers); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ---------- 并发调度：worker 池 + panic 隔离 ----------

// RunParallel 用 n 个 worker 消费 tasks，单个任务 panic 不拖垮进程。
// 返回时保证所有任务已结束（或 ctx 已取消）。
func RunParallel(ctx context.Context, n int, tasks []func(ctx context.Context)) {
	if n <= 0 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t func(ctx context.Context)) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[warn] 任务 panic 已恢复: %v\n", r)
				}
			}()
			t(ctx)
		}(t)
		if ctx.Err() != nil {
			break
		}
	}
	wg.Wait()
}

// Progress 在 stderr 打印进度（不污染 stdout 的表格/JSON）。
var progressMu sync.Mutex

func Progress(format string, a ...any) {
	progressMu.Lock()
	fmt.Fprintf(os.Stderr, format, a...)
	progressMu.Unlock()
}
