# netscope

**一站式网络体检工具**：从本机，到代理节点，再到目标站点，把"网络到底怎么样"一次测清楚。

- 拉取 Clash / v2ray 订阅，解析全部节点，**逐节点**检测可用性、延迟、丢包、速度
- 不经代理的本机网络诊断：ping、traceroute、端口探测、TLS 证书体检、DNS 审计、国内外出口 IP 与归属地
- **流媒体 / AI 解锁检测**：Netflix / Disney+ / YouTube Premium / ChatGPT / Claude / Gemini / Telegram 逐项标注（P1）
- 综合打分与推荐：按延迟 / 速度 / 可用性 / IP 质量加权，给出"最优节点 Top N"；**快照对比**看节点增删与分数变化（P1）
- 输出：终端表格、JSON、CSV、**自包含 HTML 报告**（可分享）
- Web 体检台（`serve`）：局域网内浏览器直接访问；地址清单（分组 / 导入导出）、单通道检测、**全节点对比**、评分趋势图（P1）

> 状态：**P0、P1、P2 全部实现**。`go build -o netscope .` 出单二进制即用。
>
> **触发原则（已确认）**：所有检测都是一次性、由用户手动触发的（CLI 敲命令或网页点按钮）。
> 不做长期驻留、定时巡检、自动循环与告警推送。

## 快速上手

```bash
go build -o netscope .

netscope sub check -sub "订阅链接或本地yaml"            # 节点批量可用性
netscope sub ping  -sub "订阅链接" --udp                # 延迟丢包 + STUN UDP 能力探测
netscope sub rate  -sub "订阅链接" --dur 5              # 综合评分 + HTML 报告（落 ~/.netscope/reports/）
netscope sub unlock -sub "订阅链接"                     # 流媒体/AI 解锁检测
netscope sub info  -sub "订阅链接"                      # 订阅流量剩余 / 到期时间
netscope report diff                                   # 最近两次评分快照对比
netscope ip show                                       # 国内外出口 IP 双视角 + IP 风险分
netscope dns audit github.com                          # 解析对比 + 污染检测 + EDNS 出口
netscope route bloat                                   # bufferbloat：满载下载时的延迟变化
netscope serve                                         # Web 体检台，默认 0.0.0.0:8420，局域网可访问
                                                       # 订阅直接在网页上添加（支持多个），或启动时 --sub 预置
netscope sub unlock -sub "订阅链接" --services TikTok,Spotify
netscope config init && netscope config show             # 生成/查看 YAML 配置预设
netscope report clean --keep 30 --keep-days 60           # 清理报告快照
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
5. **全部手动触发**：任何检测都一次点击 / 一条命令跑完即止，不驻留、不定时、不自动循环（已确认的非目标）
6. **克制**：明确不做的事见下

**Non-goals（不做）**：

- 不做代理客户端：不长期驻留、不转发业务流量，只在检测期间建立临时隧道
- 不做常驻监控、定时巡检与告警推送（Webhook/邮件）--原 P2 的 `sub watch`、定时巡检已从计划中移除
- 不做原生 GUI（用 `serve` 提供的 Web 界面替代）
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
netscope sub unlock   # 流媒体/AI 解锁检测（Netflix/Disney+/YouTube/ChatGPT/Claude/Gemini/Telegram）★P1
netscope sub info     # 订阅用量面板（流量剩余/使用率/到期倒计时）                      ★P1

netscope route ping   # 本机 ping（ICMP，无权限时降级 TCP ping）
netscope route trace  # 本机 TCP traceroute（无需 root）                                ★P0
netscope route bloat  # bufferbloat：满载下载时的延迟变化，判断家庭网络拥塞              ★P1
netscope port probe   # TCP/UDP 端口连通性（nc -zv 的替代）                             ★P0
netscope http inspect # URL 体检：证书链/剩余有效期/TLS 版本与套件/HTTP 版本             ★P0
netscope dns audit    # DNS 审计：多 resolver 解析对比 + 污染检测 + EDNS 递归出口        ★P1 深化
netscope ip show      # 出口 IP 体检：国内外双视角出口 IP、归属地、IP 风险分              ★P1 深化

netscope report diff  # 两次评分快照对比（节点增删、评分/延迟/速度/出口变化）            ★P1
netscope serve        # Web 体检台：报告浏览 + 地址清单（分组/导入导出）+ 全节点对比 + 趋势图 ★P0/P1
```

诊断类子命令（route/port/http/dns）加 `--via <节点名|节点序号>` 即可把探测通道切到指定节点，
默认 direct——同一套 Prober 两种视角。

## 5. 分期计划

### P0（第一版，范围已确认）

| # | 模块 | 内容 |
|---|---|---|
| 1 | CLI 骨架 + 迁移 | 子命令框架；`sub check` 与 subcheck 功能等价 |
| 2 | 延迟丢包 | `sub ping`：经节点对目标 N 次 TCP ping，输出 min/avg/max/jitter/丢包率 |
| 3 | 速度测试 | `sub speed`：Cloudflare 测速端点，经节点下载，`--upload` 同时测上传，输出 Mbps；`--size/--dur` 控制流量 |
| 4 | 本机诊断套件 | `route ping`、`route trace`（TCP traceroute）、`port probe`、`http inspect`（证书/TLS/HTTP 版本）、`dns audit`、`ip show`（国内外双视角出口 IP、归属地国家/城市/ISP） |
| 5 | HTML 报告 + 评分 | `sub rate`：一次跑多项检测 -> 加权评分 -> Top N 推荐 + 自包含 HTML 报告（内嵌 CSS，单文件可分享），快照落盘到报告目录 |
| 6 | Web 体检台 | `serve`：默认监听 `0.0.0.0:8420`（`--listen` 可改），局域网直接访问；托管报告目录（默认 `~/.netscope/reports/`）历史列表 + 在线查看；**地址清单**：网页上增删目标地址（持久化 `~/.netscope/targets.json`），一键检测--可达性/状态码、延迟、访问途径（直连或经指定节点、出口落地国家），实时进度 |

**评分模型（初稿，权重可配）**：总分 = 可用性(20) + 延迟(30) + 速度(30) + IP 质量(20)，归一化到 0-100。
IP 质量初版仅含归属地展示与 IDC/代理标记（ip-api 字段），风险评分留待 P1。

**P0 验收标准**：

- [x] `sub check`：与 subcheck 输出等价（订阅解析、多目标、排序、JSON/CSV）
- [x] `sub ping`：每节点输出 min/avg/max/jitter/loss；死节点 loss=100%
- [x] `sub speed`：输出下载 Mbps，量级与浏览器测速一致（±30%）；`--upload` 同时测上传（收尾补全）
- [x] `route trace`：逐跳输出 IP/RTT/归属地；Linux 免 root 完整逐跳，macOS 需 sudo（无权限自动降级 TCP TTL 扫描，见快速上手注记）
- [x] `port probe`：TCP open/filtered 判定正确（对照 `nc -zv`）
- [x] `http inspect`：证书剩余有效期、签发者、TLS 版本、HTTP 版本正确（对照 `openssl s_client`）
- [x] `dns audit`：多 resolver 对比出解析差异；DoH 可用性判定
- [x] `ip show`：direct 与 `--via 节点` 分别输出国内外视角出口 IP、国家/城市/ISP
- [x] `sub rate`：HTML 单文件可离线打开，含评分表与 Top N
- [x] `serve`：局域网内浏览器可访问；地址清单可增删并一键检测（可达性、延迟、访问途径）；历史报告可浏览
- [x] 所有子命令支持 `--json`；核心逻辑有单测；保留本地端到端测试
- [x] `go vet` 干净；单二进制交叉编译可过（darwin/arm64、linux/amd64、windows/amd64）

### P1（第二批，已实现）

| # | 模块 | 内容 | 状态 |
|---|---|---|---|
| 1 | `sub unlock` | 流媒体/AI 解锁检测：Netflix（含仅自制剧）、Disney+、YouTube Premium（含地区）、ChatGPT、Claude、Gemini、Telegram，逐项标注；判定端点与规则移植自 [RegionRestrictionCheck](https://github.com/lmc999/RegionRestrictionCheck) | ✅ |
| 2 | `report diff` | 两次 `sub rate` 快照对比：节点增删、评分/延迟/速度/出口变化，保留节点按 Δ分排序；无参数时自动取报告目录最新两个快照 | ✅ |
| 3 | IP 质量深化 | ip-api 的 proxy/hosting/mobile 全标记；风险分 0-100（代理 45 + 机房 35 + 移动 10）；`IPQualitySource` 可插拔数据源接口（现注册 ip-api，后续可加源）；`ip show` 与 `sub rate` 评分接入风险分 | ✅ |
| 4 | DNS 深化 | 污染检测：同一 resolver（Google）的明文 UDP 与 DoH 加密结果对比，完全不一致判定疑似在途注入；EDNS 递归出口：查 `o-o.myaddr.l.google.com` TXT，给出各 resolver 递归出口归属地与 EDNS Client Subnet | ✅ |
| 5 | `sub ping --udp` | 经节点 STUN（RFC 5389 自实现）往返测量：验证节点 UDP 能力、取回出口地址、UDP 延迟/丢包统计；Tunnel 接口增加 `ListenPacket`/`SupportsUDP`（mihomo UDP 通道） | ✅ |
| 6 | `sub info` | 订阅用量面板：`subscription-userinfo` 响应头（upload/download/total/expire，兼容毫秒时间戳），已用/剩余/使用率/到期倒计时 | ✅ |
| 7 | `route bloat` | bufferbloat：空闲基准延迟 -> N 流并发下载满载持续测延迟 -> 恢复期观察；按平均延迟增量评级 A+/A/B/C/D；ICMP 优先、无权限降级 TCP | ✅ |
| 8 | `serve` 深化 | 地址清单分组（可改分组、按组过滤）、JSON 导入导出；**全节点对比**（勾选地址 × 全部节点批量检测）；**多订阅网页管理**（添加/删除多个订阅，持久化 `~/.netscope/subs.json`，跨订阅去重，节点自动进入通道下拉）；全部由按钮手动触发（评分趋势图与历史报告展示后续按使用需求移除） | ✅ |

**P1 验收补充**：

- [x] `sub unlock`：输出 节点 × 服务 矩阵，✅ 解锁（含地区）/ 🟡 部分解锁 / ❌ 未解锁 / ⚠️ 失败；`--services` 过滤
- [x] `report diff`：新增/移除/保留三段式表格 + 平均分汇总
- [x] `dns audit`：污染检测与 EDNS 出口两节附加输出（实网验证：能识别 fake-IP 劫持/污染场景）
- [x] `sub ping --udp`：STUN 编解码有单测；direct 通道 UDP 探测有本地端到端测试
- [x] `route bloat`：三阶段流程 + 评级 + JSON 输出（实网验证通过）
- [x] `sub info`：本地文件与 HTTP 头两条路径验证（含毫秒时间戳兼容）
- [x] `serve`：分组/导入导出/全节点任务/趋势接口有 API 端到端测试

### P2（第三批，已实现）

| # | 模块 | 内容 | 状态 |
|---|---|---|---|
| 1 | YAML 配置预设 | `~/.netscope/config.yaml`（或 `--config` / `NETSCOPE_CONFIG`）：默认目标（sub check/rate）、默认解锁服务、评分权重、serve 监听/令牌/目录、快照清理策略；`config init` 生成模板、`config show` 查看生效值 | ✅ |
| 2 | 快照清理 | `report clean`（--keep/--keep-days，默认读配置）；`sub rate` 保存后自动按策略清理，html+json 成对保留 | ✅ |
| 3 | IP 数据源扩充 | `IPQualitySource` 接入 **ipwho.is** 作为 ip-api 的兜底源（限流/故障时切换；无代理标记时风险分按未知保守处理） | ✅ |
| 4 | 解锁服务扩充 | **TikTok**（首页 region 提取，区分 IDC 疑似出口；端点取自 lmc999/TikTokCheck）、**Spotify**（注册接口 status/is_country_launched 判定，含地区）；本地端到端测试覆盖判定分支 | ✅ |
| 5 | 评分权重可配 | `sub rate` 权重从配置读取，HTML 报告表头满分动态显示 | ✅ |
| 6 | serve 页面调整 | 移除评分趋势与历史报告展示（网页定位纯一：地址清单=订阅测试）；新增**本机网络体检**模块（见下） | ✅ |
| 7 | 本机网络体检 | serve 网页「🖥 本机网络体检」一键直连体检（与订阅无关，类 ip111.cn 但信息更全）：国内外双视角出口 IP/归属地/ISP/代理机房标记/风险分、基准 TCP 延迟与丢包抖动、国内外各 3 个参考站点可达性与耗时、DNS 污染检测（明文 8.8.8.8 vs Cloudflare DoH 解析对比）、出口一致性（判断分流/透明代理/TUN） | ✅ |

**P2 验收补充**：

- [x] `config init/show`：模板生成、部分权重缺项自动判无效回退默认
- [x] `report clean`：按份数/天数清理，成对删除、不动无关文件（单测覆盖）
- [x] ipwho.is：成功/失败响应解析（本地单测）；仅作兜底，ip-api 优先（保住 proxy/hosting 标记）
- [x] TikTok/Spotify：本地服务器覆盖 解锁/未解锁/疑似 IDC/解析失败 分支；实网验证判定合理
- [x] 修复 P1 遗留：`sub unlock` 的 `-timeout` 重复注册 panic

> 原 P2 的 `sub watch`（长时稳定性）与「定时巡检 + webhook/邮件推送」**已移除**：按已确认的触发原则，
> 所有检测一次性手动触发，不做长期/自动/定时形态。

## 6. 已定的技术决策

| 决策 | 内容 | 理由 |
|---|---|---|
| 语言/形态 | Go，单仓单二进制 | 可交叉编译、mihomo 是 Go 库 |
| 协议栈 | 内嵌 mihomo 作为库 | 全协议覆盖，subcheck 已验证可行 |
| IP 归属地 | ip-api.com（中文、免费），`IPQualitySource` 接口可插拔 | subcheck 已验证；风险分模型：代理 45 + 机房 35 + 移动 10 |
| 测速端点 | Cloudflare speed 端点 | 公开、全球节点多、可指定字节数（bufferbloat 也复用） |
| traceroute | TCP 方式（递增 TTL 的 SYN 探测） | 无需 root/原始套接字权限 |
| 订阅拉取 | 重试 2 次、按 type+server+port 去重 | 沿用 subcheck |
| 输出约定 | 可用优先、耗时升序；退出码：存在可用节点为 0 否则 1 | 沿用 subcheck，脚本友好 |
| 日志 | mihomo 日志静默（log.SILENT），进度走 stderr | 沿用 subcheck |
| Web 服务 | 标准库 `net/http` + `embed` 内嵌模板与静态资源，前端无构建链（原生 JS/CSS，SVG 趋势图不用图表库） | 保持单二进制零依赖；局域网直接访问 |
| serve 监听 | 默认 `0.0.0.0:8420`，`--listen` 可改；`--token` 可选鉴权 | 局域网访问是硬需求；报告含节点/出口 IP 信息，鉴权留可选项 |
| 解锁检测 | 判定端点与规则移植 RegionRestrictionCheck（Netflix 片源页 "Oh no!"、Disney+ bamgrid 三步握手、ChatGPT compliance/VPN 双探针、Claude 重定向、Gemini 页面特征、Telegram 官网页可达） | 社区长期维护、实网验证充分；不依赖其 cookies 文件，浏览器 UA 直测 |
| UDP 能力探测 | STUN Binding Request/XOR-MAPPED-ADDRESS 自实现（RFC 5389 子集），经 Tunnel.ListenPacket 走 mihomo UDP 通道 | 无第三方依赖；出口地址与往返延迟一次拿到 |
| 触发模型 | 所有检测一次性手动触发（CLI 命令 / Web 按钮），任务进度用轮询 | 用户已确认不做常驻/定时/自动检测 |

## 7. 待定问题（实现会话中决策）

1. ~~CLI 框架~~：已定标准库 flag 子命令（`parseMixed` 支持位置参数与 flag 混排）
2. ~~HTML 报告形态~~：已定 `html/template` + 内联 CSS 单文件，不引入图表库
3. ~~解锁检测的探测端点与判定规则~~：已定，见技术决策表
4. 评分权重默认值与归一化细节：P0 初版已定（20/30/30/20，IP 质量改按风险分折算），后续视使用体验调整
5. ~~`--via` 选择节点方式~~：已定名称精确匹配 / 序号（1 起）
6. ~~go module 名~~：已定 `netscope`
7. ~~`serve` 鉴权默认值~~：已定默认不鉴权，`--token` 可选
8. ~~serve 交互式检测的进度推送~~：已定轮询（700ms），无 SSE/WebSocket
9. ~~报告快照清理策略~~：已实现 `report clean` + `sub rate` 保存后自动清理（keep/keepDays）
