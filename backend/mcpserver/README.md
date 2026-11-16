# hadice-mcp：Hadice 的 MCP 服务

把 Hadice 的移动设备调试能力通过 MCP（Model Context Protocol）暴露给
WorkBuddy、Codex 等客户端，实现"连接手机 + 各项调试能力"的自动化。

## 架构：只有一条链路

```
┌────────────────────────────────────────────────────────────────┐
│ Hadice App（运行中）                                              │
│   backend.Run() → 内嵌 MCP 服务                                   │
│   · Unix socket（默认，0600）+ TCP 回环（随机端口 + token）        │
│   · 工具在这里注册与执行，Runtime 复用 GUI 的 deviceManager         │
│   · 端点写入 discovery 文件（UserConfigDir/hadice/mcp-endpoint.json）│
│   · 设置页：状态/自检/工具开关/审批/审计/客户端模板                  │
└────────────────────────────────────────────────────────────────┘
                        ▲ 桥接：工具 + 资源原样透传
┌────────────────────────────────────────────────────────────────┐
│ hadice-mcp（stdio 子进程；WorkBuddy / Codex 各自拉起一份，配置相同） │
│   不含设备能力，只做协议转换；App 未运行则明确报错退出               │
└────────────────────────────────────────────────────────────────┘
```

- 能力只有一份：工具定义与实现都在 App 内，桥接端不重复实现，因此不存在
  "两套设备状态"或"两种行为"的问题；新增工具也无需改动 `hadice-mcp`。
- 组件职责单一：`hadice-mcp` 是纯协议转换进程，不执行任何 adb/hdc 命令、
  不管理端口与设备状态；一切来自 App。
- 失败可见：App 未运行时启动即在 stderr 说明原因与下一步操作，不静默降级。
- 断线自愈：App 每次启动都是随机端口，转发失败时桥接进程会重读 discovery 文件并
  重连，因此重装/重启 App 不必重启 MCP 连接器（工具清单仍是首次连接的快照，
  新增工具需要客户端重连后才出现）。

## 传输与鉴权

| 传输 | 用途 | 鉴权 |
|------|------|------|
| Unix domain socket（默认） | stdio 桥接走这条 | socket 文件权限 `0600`；浏览器无法访问 |
| TCP `127.0.0.1`（随机端口） | 只能填 URL 的客户端 | 随机 token（`Authorization: Bearer` / `X-Hadice-Token`）+ Host/Origin 白名单 |

TCP 的三道校验对应的是一个具体威胁：**恶意网页**可以 `fetch("http://127.0.0.1:端口")`
或利用 DNS rebinding 把域名解析到回环地址，从而静默调用工具（例如 `uninstall_app`）。

- Host 必须是 `127.0.0.1` / `localhost` / `[::1]`（挡 DNS rebinding）；
- 带 Origin 头且非回环来源一律 403（挡跨站请求）；
- token 只写在 `0600` 的 discovery 文件里，网页读不到；
- `/healthz` 免 token（不返回任何能力信息，桥接用它判断 App 是否就绪）。

同用户的其它进程可以读 discovery 拿到 token——这不在防御范围内（它本来就能直接
跑 adb）。真正的护栏是**高危工具审批**：见下。

## 工具

| 工具 | 作用 | 备注 |
|------|------|------|
| `list_devices` | 列出已连接设备（adb + hdc 并行），得到后续调用要用的 `connectKey` | |
| `device_info` | 设备静态信息（型号、系统版本、ABI 等） | |
| `list_apps` | 已安装应用 + 运行状态，用于把中文应用名对到包名 | |
| `install_app` | 安装本机 APK（`-r` 覆盖安装），返回安装前后版本号；`fresh=true` 先卸载再装 | 需审批 |
| `uninstall_app` | 卸载应用（数据一并删除），系统关键包直接拒绝 | 需审批 |
| `clear_app_data` | 清除应用数据（`pm clear`），用于复现首次启动/未登录 | 需审批 |
| `launch_app` | 启动应用，返回 pid / 前台 Activity / 是否在前台；`restart=true` 先强杀（冷启动） | |
| `device_screenshot` | 截图并以图片内容返回（模型可直接「看」界面） | |
| `device_logcat` | 读取日志；`app` 按进程过滤，`durationMs` 实时采样窗口（如 10000 = 10 秒） | |
| `device_tap` / `device_swipe` / `device_back` | 点按、滑动、返回（走 `input`，不依赖投屏会话） | |

装包验收闭环：`install_app` → `launch_app` → `device_screenshot` / `device_logcat`
（`clear_app_data`、`install_app{fresh:true}` 用于复现首次启动场景）。

## 资源（resources）

| URI | 内容 |
|-----|------|
| `hadice://devices` | 当前已连接设备列表 |
| `hadice://server/status` | 服务版本/构建、端点、工具开关与审批配置 |
| `hadice://audit/recent` | 最近 50 次调用审计 |
| `hadice://device/{key}/logcat/tail` | Android 设备最近 200 条 logcat 快照 + `cursor`（资源模板） |
| `hadice://device/{key}/logcat/since/{cursor}` | cursor 之后的增量（≤500 条），循环读取即跟踪日志流（资源模板） |

资源让模型"先读状态再决定动作"，不必每次都先调 `list_devices`。

**日志流设计**（`logs.go`）：
- 每设备独立 `adb logcat` 进程 + 2000 条环形缓冲，seq 单调递增；不复用
  `adb.StartLogcatStream`（它按设备全局单例，会顶掉 GUI 日志页的流）；
- 客户端先读 `tail` 拿快照与 cursor，再循环读 `since/{cursor}` 增量跟踪；
  `truncated=true` 表示中间日志因缓冲上限被丢弃；
- 采集进程按需拉起，设备离线或 10 分钟无人读自动回收；仅支持 Android
  （鸿蒙 hilog 语义不同，读取返回明确提示）。

## 策略：开关 / 审批 / 审计

三层都在 `registerTool` 统一注入（`guard.go`），工具实现本身不感知：

1. **开关**（`settings.DisabledTools` / `settings.Enabled`）
   停用的工具调用直接返回错误并记审计。服务整体停用时关闭监听。
2. **审批**（`approval.go`）
   `install_app` / `uninstall_app` / `clear_app_data` 默认要求确认：调用在服务端挂起，
   通过 Wails 事件 `mcp:approval` 推到 Hadice 主窗口，用户点"允许"才执行。
   默认拒绝：无 UI、超时（默认 60s）、客户端断连都按拒绝处理。
   勾选"记住允许"会**持久化**到 `mcp-settings.json` 的 `approval.remembered`
   （重启 App 后仍生效）；设置页可一键清除。注意：记住的只是"不再弹窗"，
   工具仍在审批名单内——彻底免审批需在设置里把该工具移出审批清单。
3. **审计**（`audit.go`）
   每次调用写一行 JSONL 到 `<UserConfigDir>/hadice/mcp-audit.jsonl`
   （时间、工具、调用方、参数、判定、耗时、结果摘要），超过 5MB 轮转为 `.1`。

**为什么用 JSONL 而不是数据库**：单写者只追加，无需事务与索引；崩溃最多丢最后一行；
可直接 `tail -f` / `grep`；不引入 CGO 依赖。检索需求是"最近 N 条"，顺序扫 5MB 在毫秒级。
将来要做按月统计/长期留存再换 SQLite，只需替换 `audit.go` 的读写实现。

## 代码结构

```
backend/mcpserver/
├── server.go       # 元信息 + New(rt)：构建并注册工具/资源（与传输方式无关）
├── runtime.go      # Runtime：device.Manager + Store/Auditor/Approver + 端点状态
├── settings.go     # 设置的持久化（JSON，原子写）、策略查询、变更订阅
├── guard.go        # registerTool：策略 → 审批 → 执行 → 审计
├── approval.go     # 高危审批队列（Wails 事件 + 阻塞等待 + 默认拒绝 + 记忆持久化）
├── audit.go        # 审计写入/读取/轮转
├── auth.go         # TCP token + Host/Origin 校验
├── http.go         # 承载层：Unix socket + TCP（Serve/ServeHTTP）
├── status.go       # 状态聚合 + 工具目录 + 构建信息
├── resources.go    # hadice://devices、server/status、audit/recent + 日志资源模板
├── logs.go         # logcat 日志流：每设备采集进程 + 环形缓冲 + 游标增量
├── templates.go    # 16 种客户端配置模板
├── devices.go      # 设备域工具（list_devices / device_info）
├── apps.go         # 应用域工具（list_apps / launch_app / device_screenshot / device_logcat）
├── packages.go     # 包管理域工具（install_app / uninstall_app / clear_app_data）
├── input.go        # 输入域工具（device_tap / device_swipe / device_back）
├── discovery.go    # 端点发现：discovery 文件读写 + 传输匹配的 HTTP 客户端
├── bridge.go       # stdio 桥接：发现 App → 动态注册工具/资源 → 转发
├── result.go       # 工具返回值辅助
└── *_test.go       # 端到端（进程内模拟 App + 真实 stdio 子进程）与单元测试
cmd/hadice-mcp/main.go   # 接入进程入口（只做桥接）
backend/mcp_embed.go     # App 内嵌挂载点（init 自启动 + 设置变更时启停端点）
backend/mcp_service.go   # 「MCP 服务」设置页的 Wails 服务（状态/开关/审批/审计/自检）
```

## GUI 设置页

侧边栏「MCP 服务」：运行状态、端点、一键自检（含**桥接握手**：真实拉起
`hadice-mcp` 子进程完成 initialize + tools/list，模拟客户端全链路）、客户端模板
（16 种）、按域的工具开关与审批开关、调用审计（支持按工具/调用方过滤 + 清空）、
传输与阈值。高危审批弹窗是全局组件（`McpApprovalDialog`），任何页面都会弹出。

## 构建 / 测试

```bash
go build -o bin/hadice-mcp ./cmd/hadice-mcp
go test ./backend/mcpserver -v -count=1

# TestMCPBridge：进程内模拟 App + discovery + stdio 桥接全链路（工具 + 资源）
# TestMCPBridgeReconnect：App 重启换端口后，下一次调用自动重连
# TestMCPWithoutApp：App 未运行时快速失败并给出可操作提示
# TestTCPAuthRejects：无 token 401 / 错 token 401 / 非回环 Host 403 / 跨站 Origin 403
# TestUnixSocketEndToEnd：socket 权限 0600 + 经 socket 完成 tools/list
# TestSettingsPersistAndPolicy / TestApprovalApproveAndTimeout / TestAuditAppendAndRead
# TestGuardDeniesDisabledTool：停用工具的调用被拒并落审计
# TestRenderTemplates：16 份模板结构正确、JSON 可解析、HTTP 模板带 token
```

无设备连接时设备断言会 Skip，测试仍通过。

## 客户端接入

WorkBuddy / Codex 均以 stdio 子进程方式配置（各一份进程，配置完全相同）：

```json
"hadice-mcp": {
  "command": "/Applications/Hadice.app/Contents/MacOS/hadice-mcp",
  "args": ["-wait", "30s"]
}
```

- App 在运行 → 自动桥接，获得全部工具与资源；
- App 未运行 → 连接器报错，先启动 App 再重连即可（`--wait` 默认等 10s，覆盖"客户端先于 App 启动"的竞态）；
- 只能填 URL 的客户端用 `http://127.0.0.1:<port>/mcp` + `Authorization: Bearer <token>`
  （两者都在设置页可复制；App 重启后端口会变，需要重新复制）。

## 文件 / 环境变量

| 项 | 说明 |
|----|------|
| `<UserConfigDir>/hadice/mcp-endpoint.json` | discovery：传输方式、端点、socket、token、PID、构建版本 |
| `<UserConfigDir>/hadice/mcp-settings.json` | 设置（JSON，可手工编辑） |
| `<UserConfigDir>/hadice/mcp-audit.jsonl` | 审计（追加写，>5MB 轮转） |
| `<UserConfigDir>/hadice/mcp.sock` | Unix socket（0600） |
| `HADICE_MCP_ENDPOINT_FILE` | 覆盖 discovery 文件路径（测试 / 多实例用） |
| `HADICE_MCP_SETTINGS_FILE` | 覆盖设置文件路径 |
| `HADICE_MCP_AUDIT_FILE` | 覆盖审计文件路径 |
| `HADICE_MCP_DISABLED=1` | App 内不启动内嵌 MCP 服务 |
| `--wait 10s` | 等待 App 就绪的最长时间；`0` 表示只探测一次 |
