package main

// YAML 配置文件预设：目标列表、解锁项、评分权重、serve 与报告清理策略。
// 加载优先级：默认值 < ~/.netscope/config.yaml < NETSCOPE_CONFIG/--config 指定文件 < 命令行 flag。

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ScoreWeights 是综合评分的四项权重（默认 20/30/30/20，总分 100）。
type ScoreWeights struct {
	Availability float64 `yaml:"availability"`
	Latency      float64 `yaml:"latency"`
	Speed        float64 `yaml:"speed"`
	IPQ          float64 `yaml:"ipq"`
}

func (w ScoreWeights) Valid() bool {
	return w.Availability > 0 && w.Latency > 0 && w.Speed > 0 && w.IPQ > 0
}

func DefaultScoreWeights() ScoreWeights {
	return ScoreWeights{Availability: 20, Latency: 30, Speed: 30, IPQ: 20}
}

// Config 是 netscope 的全部可持久化配置。
type Config struct {
	// targets 是 sub check / sub rate 的默认检测目标（未传 -t 时使用）
	Targets []string `yaml:"targets,omitempty"`
	// unlock 是 sub unlock 的默认服务过滤（未传 --services 时使用；空 = 全部）
	Unlock struct {
		Services []string `yaml:"services,omitempty"`
	} `yaml:"unlock,omitempty"`
	// score 是 sub rate 的评分权重
	Score ScoreWeights `yaml:"score,omitempty"`
	// serve 是 Web 体检台的默认参数
	Serve struct {
		Listen      string `yaml:"listen,omitempty"`
		Token       string `yaml:"token,omitempty"`
		ReportDir   string `yaml:"reportDir,omitempty"`
		TargetsFile string `yaml:"targetsFile,omitempty"`
		SubsFile    string `yaml:"subsFile,omitempty"`
	} `yaml:"serve,omitempty"`
	// reports 是快照清理策略（keep 保留份数、keepDays 保留天数；0 = 不限）
	Reports struct {
		Keep     int `yaml:"keep,omitempty"`
		KeepDays int `yaml:"keepDays,omitempty"`
	} `yaml:"reports,omitempty"`
}

// appConfig 是全局生效的配置（main 里加载，各命令读取）。
var appConfig = &Config{}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "netscope.yaml"
	}
	return filepath.Join(home, ".netscope", "config.yaml")
}

// loadConfig 读取并规范化配置；文件不存在返回零值配置（非错误）。
func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if !cfg.Score.Valid() {
		cfg.Score = ScoreWeights{}
	}
	return cfg, nil
}

// configTemplate 生成带注释的示例配置。
func configTemplate() string {
	return `# netscope 配置预设（放到 ~/.netscope/config.yaml 或 --config 指定路径）
# 命令行 flag 优先级高于本文件。

# sub check / sub rate 的默认检测目标（未传 -t 时使用）
targets:
  - https://www.gstatic.com/generate_204
  - https://www.youtube.com/generate_204

# sub unlock 的默认服务过滤（未传 --services 时使用；空或不写 = 全部）
unlock:
  services:
    - Netflix
    - ChatGPT

# sub rate 评分权重（默认 20/30/30/20，总分 100）
score:
  availability: 20
  latency: 30
  speed: 30
  ipq: 20

# Web 体检台默认参数
serve:
  listen: 0.0.0.0:8420
  token: ""
  # reportDir: ~/.netscope/reports
  # targetsFile: ~/.netscope/targets.json
  # subsFile: ~/.netscope/subs.json

# 报告快照清理（sub rate 保存后自动执行；0 = 不限）
reports:
  keep: 50      # 最多保留多少份（html+json 成对算一份）
  keepDays: 90  # 最多保留多少天
`
}

// cmdConfigInit 生成示例配置文件。
func cmdConfigInit(_ []string) int {
	path := globalConfigPath
	if _, err := os.Stat(path); err == nil {
		fatalf("%s 已存在，不覆盖（先删除或换 --config 路径）", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(path, []byte(configTemplate()), 0o644); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("已生成示例配置：%s\n按需编辑后直接运行各命令即可生效。\n", path)
	return 0
}

// cmdConfigShow 打印当前生效的配置。
func cmdConfigShow(_ []string) int {
	fmt.Printf("配置文件：%s\n\n", globalConfigPath)
	out, err := yaml.Marshal(appConfig)
	if err != nil {
		fatalf("%v", err)
	}
	s := string(out)
	if s == "{}\n" {
		s = "#（全为默认值，尚未配置；netscope config init 可生成模板）\n"
	}
	fmt.Print(s)
	return 0
}

// orDefault 返回第一个非空字符串。
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
