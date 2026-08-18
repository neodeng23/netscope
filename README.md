# netscope

**一站式网络体检工具**：从本机，到代理节点，再到目标站点，把"网络到底怎么样"一次测清楚。

- 拉取 Clash / v2ray 订阅，解析全部节点，**逐节点**检测可用性、延迟、丢包、速度
- 不经代理的本机网络诊断：ping、traceroute、端口探测、TLS 证书体检、DNS 审计、国内外出口 IP 与归属地
- 综合打分与推荐：按延迟 / 速度 / 可用性 / IP 质量加权，给出"最优节点 Top N"
- 输出：终端表格、JSON、CSV、**自包含 HTML 报告**（可分享）
- Web 体检台（`serve`）：局域网内浏览器直接访问；**自定义地址清单**——网页上添加地址，一键测可达性、延迟、访问途径（直连/经节点、落地国家）

> 状态：**P0 已实现**。`go build -o netscope .` 出单二进制即用；P1/P2 见分期计划。

## 快速上手

```bash
go build -o netscope .

netscope sub check -sub "订阅链接或本地yaml"            # 节点批量可用性
netscope sub rate -sub "订阅链接" --dur 5              # 综合评分 + HTML 报告（落 ~/.netscope/reports/）
netscope ip show                                       # 国内外出口 IP 双视角
netscope serve --sub "订阅链接"                         # Web 体检台，默认 0.0.0.0:8420，局域网可访问
```

> 注：`route trace` 完整逐跳（含中间路由 IP 与归属地）需要能收 ICMP 差错报文：Linux 免 root（UDP errqueue），
> macOS 需 `sudo`（raw socket）；无权限时自动降级为 TCP TTL 扫描（仅判断第几跳可达）。

---

## 1. 背景与起点

netscope 由 `subcheck`（`~/code/subcheck`，已可用并经实网验证）重建而来。subcheck 只做一件事：
拉订阅、逐节点访问指定 URL、报告状态码/耗时/出口 IP。

**直接复用的资产**（迁移而非重写）：

| 资产 | 位置 | 说明 |
|---|---|---|
| 订阅接入 | `sub.go` | Clash YAML + v2ray Base64/明文双格式、本地文件、多订阅去重、重试 |
| 节点检测 | `check.go` | 经节点建连、HTTP 检测、出口 IP 归属地 |
| 报告输出 | `report.go` | 终端表格（中文对齐）、JSON、CSV |
| 测试 | `main_test.go` | 含本地端到端（direct / HTTP 代理 CONNECT / 不可达 / 非法配置） |

**沿用已验证的技术底座**：协议解析内嵌 [mihomo（Clash.Meta）](https://github.com/MetaCubeX/mihomo) 内核作为库，
天然支持 ss / ssr / vmess / vless / trojan / hysteria2 / tuic / wireguard / socks5 / http 等协议，
无需自己实现任何代理协议。

## 2. 设计原则

1. **单二进制、零依赖**：`go build` 出一个可执行文件即可用；不依赖系统命令（ping/traceroute/nc 都自己实现）
2. **检测器可插拔**：所有检测面向统一的隧道接口（direct 或任一节点），新增检测项不改核心调度
3. **人读与机器读并重**：终端表格 / HTML 给人看，JSON / CSV 给脚本用，每个子命令都支持 `--json`
4. **中文优先**：CLI 输出与报告以中文为主
5. **克制**：明确不做的事见下

**Non-goals（不做）**：

- 不做代理客户端：不长期驻留、不转发业务流量，只在检测期间建立临时隧道
- P0/P1 不做常驻监控与告警推送（P2 再议）
- 不做原生 GUI（用 `serve` 提供的 Web 界面替代，只读起步，交互式检测放 P1）
- 不做攻击性扫描：端口探测仅限用户显式指定的目标

## 3. 总体架构

```
┌─ 接入层 ──────┐   ┌─ 隧道层 ────────┐   ┌─ 检测层 ────────────┐   ┌─ 报告层 ─────────┐
│ 订阅链接/文件  │   │ direct（本机）    │   │ Prober（可插拔）      │   │ 表格/JSON/CSV     │
│ 单条节点 URI   │ → │ mihomo 节点隧道   │ → │ latency / speed /   │ → │ HTML 报告         │
│ Clash YAML    │   │ （按需建、测完关） │   │ http / tls / port / │   │ 综合评分 + Top N  │
│ v2ray Base64  │   │                  │   │ dns / traceroute …  │   │ 快照 diff (P1)    │
│               │   │                  │   │                     │   │ Web 服务 (serve)  │
└───────────────┘   └──────────────────┘   └─────────────────────┘   └──────────────────┘
```

核心抽象（示意）：

```go
// Tunnel: 一条可复用的探测通道，direct 或某个节点
type Tunnel interface {
    DialContext(ctx, network, addr) (net.Conn, error) // 经隧道建连
    Name() string                                      // "direct" 或节点名
    Meta() NodeMeta                                    // 类型/服务器等（direct 为空）
}

// Prober: 一项检测。面向 Tunnel 编程，不关心流量走直连还是节点
type Prober interface {
    Name() string
    Run(ctx context.Context, t Tunnel, opts RunOpts) (Result, error)
}
```

- **并发调度**：worker 池 + 信号量（沿用 subcheck 模式），支持 Ctrl-C 中断并输出已完成部分
- **进程内安全隔离**：单个节点检测 panic 不得拖垮进程（沿用 subcheck 的 recover 包装）

## 4. CLI 形态

```
netscope sub check    # 节点批量可用性（多目标 HTTP 状态码、建连/总耗时、出口 IP）★subcheck 迁移
netscope sub ping     # 延迟与丢包：TCP ping N 次，min/avg/max/抖动/丢包率            ★P0
netscope sub speed    # 速度测试：经节点下载/上传测带宽                                 ★P0
netscope sub rate     # 综合打分：跑多项检测，加权评分，输出 Top N 推荐 + HTML 报告      ★P0

netscope route ping   # 本机 ping（ICMP，无权限时降级 TCP ping）
netscope route trace  # 本机 TCP traceroute（无需 root）                                ★P0
netscope port probe   # TCP/UDP 端口连通性（nc -zv 的替代）                             ★P0
netscope http inspect # URL 体检：证书链/剩余有效期/TLS 版本与套件/HTTP 版本             ★P0
netscope dns audit    # DNS 审计：多 resolver 解析对比、DoH 可用性                      ★P0
netscope ip show      # 出口 IP 体检：国内外双视角出口 IP、归属地（国家/城市/ISP）        ★P0

netscope sub unlock   # 流媒体/AI 解锁检测（Netflix/Disney+/ChatGPT/Claude/Gemini…）   P1
netscope report diff  # 两次检测结果对比                                                P1
netscope sub watch    # 稳定性长测（循环探测观察波动）                                    P2
netscope serve        # Web 体检台：报告浏览 + 地址清单（添加地址、测可达/延迟/访问途径）  ★P0
```

诊断类子命令（route/port/http/dns）加 `--via <节点名|节点序号>` 即可把探测通道切到指定节点，
默认 direct——同一套 Prober 两种视角。

## 5. 分期计划

### P0（第一版，范围已确认）

| # | 模块 | 内容 |
|---|---|---|
| 1 | CLI 骨架 + 迁移 | 子命令框架；`sub check` 与 subcheck 功能等价 |
| 2 | 延迟丢包 | `sub ping`：经节点对目标 N 次 TCP ping，输出 min/avg/max/jitter/丢包率 |
| 3 | 速度测试 | `sub speed`：Cloudflare 测速端点，经节点下载（上传可选），输出 Mbps；`--size/--duration` 控制流量 |
| 4 | 本机诊断套件 | `route ping`、`route trace`（TCP traceroute）、`port probe`、`http inspect`（证书/TLS/HTTP 版本）、`dns audit`、`ip show`（国内外双视角出口 IP、归属地国家/城市/ISP） |
| 5 | HTML 报告 + 评分 | `sub rate`：一次跑多项检测 -> 加权评分 -> Top N 推荐 + 自包含 HTML 报告（内嵌 CSS，单文件可分享），快照落盘到报告目录 |
| 6 | Web 体检台 | `serve`：默认监听 `0.0.0.0:8420`（`--listen` 可改），局域网直接访问；托管报告目录（默认 `~/.netscope/reports/`）历史列表 + 在线查看；**地址清单**：网页上增删目标地址（持久化 `~/.netscope/targets.json`），一键检测--可达性/状态码、延迟、访问途径（直连或经指定节点、出口落地国家），实时进度 |

**评分模型（初稿，权重可配）**：总分 = 可用性(20) + 延迟(30) + 速度(30) + IP 质量(20)，归一化到 0-100。
IP 质量初版仅含归属地展示与 IDC/代理标记（ip-api 字段），风险评分留待 P1。

**P0 验收标准**：

- [ ] `sub check`：与 subcheck 输出等价（订阅解析、多目标、排序、JSON/CSV）
- [ ] `sub ping`：每节点输出 min/avg/max/jitter/loss；死节点 loss=100%
- [ ] `sub speed`：输出下载 Mbps，量级与浏览器测速一致（±30%）
- [ ] `route trace`：逐跳输出 IP/RTT/归属地，无需 root
- [ ] `port probe`：TCP open/filtered 判定正确（对照 `nc -zv`）
- [ ] `http inspect`：证书剩余有效期、签发者、TLS 版本、HTTP 版本正确（对照 `openssl s_client`）
- [ ] `dns audit`：多 resolver 对比出解析差异；DoH 可用性判定
- [ ] `ip show`：direct 与 `--via 节点` 分别输出国内外视角出口 IP、国家/城市/ISP
- [ ] `sub rate`：HTML 单文件可离线打开，含评分表与 Top N
- [ ] `serve`：局域网内浏览器可访问；地址清单可增删并一键检测（可达性、延迟、访问途径）；历史报告可浏览
- [ ] 所有子命令支持 `--json`；核心逻辑有单测；保留本地端到端测试
- [ ] `go vet` 干净；单二进制交叉编译可过（至少 darwin/arm64、linux/amd64）

### P1（第二批）

- `sub unlock`：流媒体/AI 解锁检测（Netflix 原生/自制、Disney+、YouTube、ChatGPT/OpenAI、Claude、Gemini、Telegram），逐项标注
- `report diff`：两次检测快照对比（节点增删、延迟变化、评分变化）
- IP 质量深化：机房/家宽/代理标记细化、风险评分（可插拔数据源）
- DNS 深化：污染特征检测、EDNS 出口归属
- `sub ping` 的 UDP 能力探测（经节点 STUN/DNS-UDP）
- `sub info`：订阅用量面板（流量剩余/到期倒计时，来自 `subscription-userinfo` 响应头）
- bufferbloat：满载下载时的延迟变化（负载延迟），判断家庭网络拥塞
- `serve` 深化：多次快照延迟/评分趋势图（SVG）；地址清单支持分组与导入导出、批量挂全部节点对比

### P2（远期）

- `sub watch`：长时稳定性测试（波动、掉线时间线）
- 定时巡检 + webhook/邮件推送
- YAML 配置文件预设（目标列表、解锁项、评分权重持久化）

## 6. 已定的技术决策

| 决策 | 内容 | 理由 |
|---|---|---|
| 语言/形态 | Go，单仓单二进制 | 可交叉编译、mihomo 是 Go 库 |
| 协议栈 | 内嵌 mihomo 作为库 | 全协议覆盖，subcheck 已验证可行 |
| IP 归属地 | ip-api.com（中文、免费），可关闭 | subcheck 已验证；更细风控留 P1 接口 |
| 测速端点 | Cloudflare speed 端点 | 公开、全球节点多、可指定字节数 |
| traceroute | TCP 方式（递增 TTL 的 SYN 探测） | 无需 root/原始套接字权限 |
| 订阅拉取 | 重试 2 次、按 type+server+port 去重 | 沿用 subcheck |
| 输出约定 | 可用优先、耗时升序；退出码：存在可用节点为 0 否则 1 | 沿用 subcheck，脚本友好 |
| 日志 | mihomo 日志静默（log.SILENT），进度走 stderr | 沿用 subcheck |
| Web 服务 | 标准库 `net/http` + `embed` 内嵌模板与静态资源，前端无构建链（原生 JS/CSS） | 保持单二进制零依赖；局域网直接访问 |
| serve 监听 | 默认 `0.0.0.0:8420`，`--listen` 可改；`--token` 可选鉴权 | 局域网访问是硬需求；报告含节点/出口 IP 信息，鉴权留可选项 |

## 7. 待定问题（实现会话中决策）

1. CLI 框架：标准库 flag 子命令 vs cobra（倾向标准库，保持零依赖）
2. HTML 报告形态：内嵌 `html/template` + 内联 CSS 单文件（倾向）；是否引入图表库（倾向不引入，用 CSS 条形图）
3. 解锁检测的探测端点与判定规则（P1 前调研，参考 RegionRestrictionCheck 的做法）
4. 评分权重默认值与归一化细节
5. 诊断子命令的 `--via` 选择节点方式：名称 / 序号 / 交互式选择
6. go module 名（`netscope` 或带 host 的完整路径）
7. `serve` 鉴权默认值：纯内网信任不鉴权 vs 首次启动生成随机 token（报告含节点与出口 IP 信息）
8. `serve` 交互式检测（P1）的进度推送方式：SSE vs WebSocket vs 轮询
9. 报告快照清理策略（按份数/天数保留）与报告目录布局
