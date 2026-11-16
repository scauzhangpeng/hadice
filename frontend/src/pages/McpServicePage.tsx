import React, { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Server,
  RefreshCw,
  Copy,
  Check,
  ShieldCheck,
  ShieldAlert,
  Plug,
  ListChecks,
  Activity,
  Save
} from 'lucide-react'
import { toast } from 'sonner'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { WindowToggleButton } from '@/components/layout/WindowToggleButton'
import { copyTextToClipboard } from '@/lib/clipboard'
import * as MCPService from '../../bindings/Hadice/backend/mcpservice'

/* ------------------------------- 类型 ------------------------------- */

interface ToolStatus {
  name: string
  domain: string
  highRisk: boolean
  enabled: boolean
  needsApproval: boolean
  calls: number
}

interface AuditEntry {
  time: string
  tool: string
  client?: string
  session?: string
  args?: string
  decision: string
  approval?: string
  durationMs: number
  isError: boolean
  summary?: string
}

interface ApprovalRequest {
  id: string
  tool: string
  client?: string
  args?: string
  createdAt: string
  timeoutSec: number
}

interface Status {
  running: boolean
  enabled: boolean
  version: string
  build: string
  startedAt: string
  uptimeSec: number
  tcp?: string
  socket?: string
  token?: string
  toolCount: number
  tools: ToolStatus[]
  pending: ApprovalRequest[]
  remembered: string[]
  auditPath: string
  settingsPath: string
  recentErrors: AuditEntry[]
  recentCalls: AuditEntry[]
}

interface Settings {
  enabled: boolean
  transport: { tcp: boolean; unixSocket: boolean; fixedPort: number }
  disabledTools: string[]
  approval: { enabled: boolean; tools: string[]; timeoutSec: number }
  audit: { enabled: boolean }
}

interface ClientTemplate {
  id: string
  name: string
  format: string
  path: string
  note: string
  content: string
}

interface CheckItem {
  name: string
  ok: boolean
  detail: string
  hint?: string
}

interface Overview {
  status: Status
  settings: Settings
  templates: ClientTemplate[]
  bridgePath: string
  bridgeExists: boolean
  clients: { client: string; calls: number; last: string }[]
}

const EMPTY_STATUS: Status = {
  running: false,
  enabled: false,
  version: '',
  build: '',
  startedAt: '',
  uptimeSec: 0,
  toolCount: 0,
  tools: [],
  pending: [],
  remembered: [],
  auditPath: '',
  settingsPath: '',
  recentErrors: [],
  recentCalls: []
}

/* ----------------------------- 小组件 ----------------------------- */

function CopyButton({ text, label }: { text: string; label?: string }): React.JSX.Element {
  const [copied, setCopied] = useState(false)
  return (
    <Button
      variant="outline"
      size="sm"
      className="shrink-0 gap-1.5"
      onClick={async () => {
        if (!(await copyTextToClipboard(text))) return
        setCopied(true)
        setTimeout(() => setCopied(false), 1500)
      }}
    >
      {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
      {label ?? (copied ? '已复制' : '复制')}
    </Button>
  )
}

function decisionBadge(decision: string): React.JSX.Element {
  const map: Record<string, { text: string; variant: 'success' | 'warning' | 'destructive' | 'secondary' }> = {
    ok: { text: '成功', variant: 'success' },
    error: { text: '失败', variant: 'destructive' },
    'denied:policy': { text: '已停用', variant: 'secondary' },
    'denied:approval': { text: '用户拒绝', variant: 'warning' },
    timeout: { text: '超时拒绝', variant: 'warning' },
    'denied:disabled': { text: '服务停用', variant: 'secondary' }
  }
  const it = map[decision] ?? { text: decision, variant: 'secondary' as const }
  return <Badge variant={it.variant}>{it.text}</Badge>
}

function formatTime(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

/* ------------------------------- 页面 ------------------------------- */

export function McpServicePage(): React.JSX.Element {
  const [ov, setOv] = useState<Overview | null>(null)
  const [loading, setLoading] = useState(false)
  const [checking, setChecking] = useState(false)
  const [checks, setChecks] = useState<CheckItem[] | null>(null)
  const [tplId, setTplId] = useState('workbuddy')
  const [audit, setAudit] = useState<AuditEntry[]>([])
  const [auditTool, setAuditTool] = useState('')
  const [auditClient, setAuditClient] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [portDraft, setPortDraft] = useState('')
  const [timeoutDraft, setTimeoutDraft] = useState('')

  const load = useCallback(
    async (quiet = false) => {
      if (!quiet) setLoading(true)
      try {
        const data = (await MCPService.GetMCPOverview()) as unknown as Overview
        setOv(data)
        setError(null)
        setPortDraft(String(data.settings.transport.fixedPort || ''))
        setTimeoutDraft(String(data.settings.approval.timeoutSec || 60))
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!quiet) setLoading(false)
      }
    },
    []
  )

  useEffect(() => {
    load()
    const timer = setInterval(() => load(true), 5000)
    return () => clearInterval(timer)
  }, [load])

  const loadAudit = useCallback(async () => {
    try {
      const list = (await MCPService.ListMCPAudit(100)) as unknown as AuditEntry[]
      setAudit(list || [])
    } catch {
      /* 审计读取失败不影响主界面 */
    }
  }, [])

  useEffect(() => {
    loadAudit()
  }, [loadAudit])

  const auditTools = useMemo(() => Array.from(new Set(audit.map((e) => e.tool))).sort(), [audit])
  const auditClients = useMemo(
    () => Array.from(new Set(audit.map((e) => e.client || '-'))).sort(),
    [audit]
  )
  const auditFiltered = useMemo(
    () =>
      audit.filter(
        (e) =>
          (!auditTool || e.tool === auditTool) &&
          (!auditClient || (e.client || '-') === auditClient)
      ),
    [audit, auditTool, auditClient]
  )

  const status = ov?.status ?? EMPTY_STATUS
  const settings = ov?.settings
  const templates = ov?.templates ?? []
  const tpl = useMemo(
    () => templates.find((t) => t.id === tplId) ?? templates[0],
    [templates, tplId]
  )

  const domains = useMemo(() => {
    const m = new Map<string, ToolStatus[]>()
    for (const t of status.tools) {
      if (!m.has(t.domain)) m.set(t.domain, [])
      m.get(t.domain)!.push(t)
    }
    return Array.from(m.entries())
  }, [status.tools])

  const runCheck = async () => {
    setChecking(true)
    try {
      const res = (await MCPService.SelfCheckMCP()) as unknown as { ok: boolean; items: CheckItem[] }
      setChecks(res.items || [])
      toast[res.ok ? 'success' : 'warning'](res.ok ? '自检通过' : '自检发现问题，见下方明细')
    } catch (err) {
      toast.error(`自检失败：${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setChecking(false)
      load(true)
    }
  }

  const toggleTool = async (name: string, enabled: boolean) => {
    try {
      await MCPService.SetMCPToolEnabled(name, enabled)
      await load(true)
    } catch (err) {
      toast.error(`保存失败：${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const toggleApproval = async (name: string, required: boolean) => {
    try {
      await MCPService.SetMCPToolApproval(name, required)
      await load(true)
    } catch (err) {
      toast.error(`保存失败：${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const saveAdvanced = async () => {
    if (!settings) return
    const port = parseInt(portDraft, 10)
    const to = parseInt(timeoutDraft, 10)
    try {
      await MCPService.UpdateMCPSettings({
        ...settings,
        transport: {
          ...settings.transport,
          fixedPort: Number.isFinite(port) ? port : 0
        },
        approval: {
          ...settings.approval,
          timeoutSec: Number.isFinite(to) ? to : 60
        }
      } as never)
      toast.success('已保存（传输层改动需重启 App 生效）')
      await load(true)
    } catch (err) {
      toast.error(`保存失败：${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const setTransport = async (key: 'tcp' | 'unixSocket', value: boolean) => {
    if (!settings) return
    try {
      await MCPService.UpdateMCPSettings({
        ...settings,
        transport: { ...settings.transport, [key]: value }
      } as never)
      await load(true)
    } catch (err) {
      toast.error(`保存失败：${err instanceof Error ? err.message : String(err)}`)
    }
  }

  return (
    <div className="p-4 sm:p-6 h-full flex flex-col min-w-0">
      {/* 标题栏 */}
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3 flex-shrink-0">
        <h1 className="text-2xl font-bold flex items-center gap-3">
          <Server className="h-6 w-6 text-primary" />
          MCP 服务
          <WindowToggleButton />
        </h1>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={runCheck} disabled={checking} className="gap-2">
            <ShieldCheck className={`h-4 w-4 ${checking ? 'animate-pulse' : ''}`} />
            重新检查
          </Button>
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading} className="gap-2">
            <RefreshCw className={`h-4 w-4 ${loading ? 'animate-spin' : ''}`} />
            刷新
          </Button>
        </div>
      </div>

      {error && (
        <Card className="mb-4 border-destructive/40 flex-shrink-0">
          <CardContent className="p-3 text-sm text-destructive break-all">{error}</CardContent>
        </Card>
      )}

      {/*
        不用 Radix ScrollArea：它给内容包一层 display:table 的容器，
        任何比视口宽的块（例如配置预览的 <pre>）都会把整页撑宽，
        而 Root 是 overflow-hidden 且只渲染竖向滚动条 —— 结果就是右边
        的 TCP 端点和「复制」按钮在窄窗口下被裁掉且滚不到。
        普通 div 走块级布局，内容按外层宽度收缩，天然自适应。
      */}
      <div className="flex-1 min-h-0 min-w-0 overflow-y-auto">
        <div className="space-y-4 pr-1 pb-6 min-w-0">
          {/* 状态 */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-base flex flex-wrap items-center gap-2">
                <Activity className="h-4 w-4 text-primary" />
                运行状态
                {status.running && status.enabled ? (
                  <Badge variant="success">运行中</Badge>
                ) : (
                  <Badge variant="warning">已停用</Badge>
                )}
                {checking ? null : status.pending.length > 0 && (
                  <Badge variant="warning">{status.pending.length} 个待审批</Badge>
                )}
              </CardTitle>
              <CardDescription>
                MCP 服务内嵌在 Hadice App 里，App 运行即对外提供；工具能力与界面共用同一个设备管理器。
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-3 text-sm">
              {/* lg 而非 md 才分两栏：窗口（含 160px 侧栏）在 1024 以下时两栏各剩不到 300px，
                  端点路径和复制按钮会挤成难看的窄条，竖排更清晰 */}
              <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                <div className="min-w-0">
                  <div className="text-xs text-muted-foreground mb-1">Unix socket（stdio 桥默认）</div>
                  <div className="flex items-start gap-2">
                    <code
                      className="min-w-0 flex-1 break-all whitespace-pre-wrap rounded bg-muted/60 px-2 py-1 text-xs leading-relaxed"
                      title={status.socket || '未启用'}
                    >
                      {status.socket || '未启用'}
                    </code>
                    {status.socket && <CopyButton text={status.socket} />}
                  </div>
                </div>
                <div className="min-w-0">
                  <div className="text-xs text-muted-foreground mb-1">TCP 端点（仅 URL 型客户端）</div>
                  <div className="flex items-start gap-2">
                    <code
                      className="min-w-0 flex-1 break-all whitespace-pre-wrap rounded bg-muted/60 px-2 py-1 text-xs leading-relaxed"
                      title={status.tcp || '未启用'}
                    >
                      {status.tcp || '未启用'}
                    </code>
                    {status.tcp && <CopyButton text={status.tcp} />}
                  </div>
                </div>
              </div>

              <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-xs text-muted-foreground">
                <span>版本 {status.version}（{status.build || 'dev'}）</span>
                <span>已运行 {Math.floor(status.uptimeSec / 60)} 分钟</span>
                <span>{status.toolCount} 个工具</span>
                {status.remembered.length > 0 && (
                  <span className="flex items-center gap-2">
                    已记住允许（持久）：{status.remembered.join('、')}
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-6 px-2 text-xs"
                      onClick={async () => {
                        await MCPService.ClearMCPRemembered()
                        load(true)
                      }}
                    >
                      清除
                    </Button>
                  </span>
                )}
              </div>
            </CardContent>
          </Card>

          {/* 自检 */}
          {checks && (
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-base flex items-center gap-2">
                  <ListChecks className="h-4 w-4 text-primary" />
                  自检结果
                </CardTitle>
                <CardDescription>
                  逐项检查链路：服务 → discovery 文件 → socket 权限 → TCP 鉴权 → 桥接二进制 → 桥接握手 → 审计写入。
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-2">
                {checks.map((c) => (
                  <div key={c.name} className="flex items-start gap-3 text-sm">
                    {c.ok ? (
                      <Check className="mt-0.5 h-4 w-4 shrink-0 text-green-500" />
                    ) : (
                      <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0 text-yellow-500" />
                    )}
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{c.name}</span>
                        {!c.ok && c.hint && (
                          <span className="text-xs text-muted-foreground">→ {c.hint}</span>
                        )}
                      </div>
                      <div className="text-xs text-muted-foreground break-all">{c.detail}</div>
                    </div>
                  </div>
                ))}
              </CardContent>
            </Card>
          )}

          {/* 接入方式 */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-base flex items-center gap-2">
                <Plug className="h-4 w-4 text-primary" />
                接入方式
              </CardTitle>
              <CardDescription>
                推荐 stdio：客户端拉起 <code>hadice-mcp</code>，它自动发现运行中的 App，App 重启换端口也不用改配置。
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="flex items-start gap-2">
                <span className="text-xs text-muted-foreground shrink-0">桥接二进制</span>
                <code
                  className="min-w-0 flex-1 break-all rounded bg-muted/60 px-2 py-1 text-xs leading-relaxed"
                  title={ov?.bridgePath || '-'}
                >
                  {ov?.bridgePath || '-'}
                </code>
                {ov?.bridgePath && <CopyButton text={ov.bridgePath} />}
                {ov && !ov.bridgeExists && <Badge variant="warning">未找到</Badge>}
              </div>

              <div className="flex items-center gap-2">
                <span className="text-xs text-muted-foreground shrink-0">客户端</span>
                <select
                  className="h-8 min-w-0 flex-1 rounded-md border border-input bg-background px-2 text-sm"
                  value={tpl?.id ?? ''}
                  onChange={(e) => setTplId(e.target.value)}
                >
                  {templates.map((t) => (
                    <option key={t.id} value={t.id}>
                      {t.name}（{t.format}）
                    </option>
                  ))}
                </select>
                {tpl && <CopyButton text={tpl.content} label="复制配置" />}
              </div>

              {tpl && (
                <>
                  <div className="text-xs text-muted-foreground break-all">
                    配置文件：<code>{tpl.path}</code>
                  </div>
                  <div className="text-xs text-muted-foreground">{tpl.note}</div>
                  <pre className="max-h-64 w-full min-w-0 overflow-auto whitespace-pre-wrap break-all rounded-md border border-border bg-muted/40 p-3 text-xs">
                    {tpl.content}
                  </pre>
                </>
              )}
            </CardContent>
          </Card>

          {/* 工具开关 */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-base flex items-center gap-2">
                <ShieldCheck className="h-4 w-4 text-primary" />
                能力与审批
              </CardTitle>
              <CardDescription>
                停用的工具调用会被直接拒绝；勾选“需确认”的工具在调用时会弹出确认框（默认拒绝）。
                停用不影响已连接的客户端清单，需重连客户端才会刷新。
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              {domains.map(([domain, list]) => (
                <div key={domain}>
                  <div className="mb-2 text-xs font-semibold text-muted-foreground">{domain}</div>
                  <div className="space-y-1">
                    {list.map((t) => (
                      <div
                        key={t.name}
                        className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md px-2 py-1.5 hover:bg-accent/40"
                      >
                        <code className="w-32 shrink-0 truncate text-xs sm:w-44" title={t.name}>
                          {t.name}
                        </code>
                        {t.highRisk && <Badge variant="warning">高危</Badge>}
                        <span className="min-w-0 flex-1 text-xs text-muted-foreground">
                          {t.calls > 0 ? `已调用 ${t.calls} 次` : '尚未调用'}
                        </span>
                        <label className="flex shrink-0 items-center gap-1.5 text-xs">
                          <Checkbox
                            checked={t.needsApproval}
                            onCheckedChange={(v) => toggleApproval(t.name, v === true)}
                          />
                          需确认
                        </label>
                        <label className="flex shrink-0 items-center gap-1.5 text-xs">
                          <Checkbox
                            checked={t.enabled}
                            onCheckedChange={(v) => toggleTool(t.name, v === true)}
                          />
                          启用
                        </label>
                      </div>
                    ))}
                  </div>
                </div>
              ))}
            </CardContent>
          </Card>

          {/* 审计 */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-base flex items-center gap-2">
                <ListChecks className="h-4 w-4 text-primary" />
                调用审计
              </CardTitle>
              <CardDescription className="break-all">
                追加写在本机 JSONL 文件：<code>{status.auditPath || '-'}</code>
                {ov?.clients?.length ? ` ｜ 调用方：${ov.clients.map((c) => `${c.client}(${c.calls})`).join('、')}` : ''}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="flex flex-wrap items-center gap-2 mb-2">
                <span className="text-xs text-muted-foreground">过滤</span>
                <select
                  className="h-8 rounded-md border border-input bg-background px-2 text-xs"
                  value={auditTool}
                  onChange={(e) => setAuditTool(e.target.value)}
                >
                  <option value="">全部工具</option>
                  {auditTools.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
                <select
                  className="h-8 rounded-md border border-input bg-background px-2 text-xs"
                  value={auditClient}
                  onChange={(e) => setAuditClient(e.target.value)}
                >
                  <option value="">全部调用方</option>
                  {auditClients.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
                {(auditTool || auditClient) && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-7 px-2 text-xs"
                    onClick={() => {
                      setAuditTool('')
                      setAuditClient('')
                    }}
                  >
                    清除过滤
                  </Button>
                )}
                <span className="ml-auto text-xs text-muted-foreground">
                  {auditFiltered.length} 条
                </span>
              </div>

              <Tabs defaultValue="all">
                <div className="flex flex-wrap items-center justify-between gap-2 mb-2">
                  <TabsList>
                    <TabsTrigger value="all">最近调用</TabsTrigger>
                    <TabsTrigger value="errors">异常 / 拒绝</TabsTrigger>
                  </TabsList>
                  <div className="flex gap-2">
                    <Button variant="ghost" size="sm" onClick={loadAudit}>
                      刷新
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={async () => {
                        await MCPService.ClearMCPAudit()
                        setAudit([])
                        setAuditTool('')
                        setAuditClient('')
                        toast.success('审计已清空')
                      }}
                    >
                      清空
                    </Button>
                  </div>
                </div>

                <TabsContent value="all">
                  <AuditList entries={auditFiltered} />
                </TabsContent>
                <TabsContent value="errors">
                  <AuditList entries={auditFiltered.filter((e) => e.decision !== 'ok')} />
                </TabsContent>
              </Tabs>
            </CardContent>
          </Card>

          {/* 高级 */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-base flex items-center gap-2">
                <Server className="h-4 w-4 text-primary" />
                传输与阈值
              </CardTitle>
              <CardDescription>传输层改动需重启 App 生效；超时与审批即时生效。</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3 text-sm">
              <div className="flex flex-wrap items-center gap-6">
                <label className="flex items-center gap-2">
                  <Checkbox
                    checked={!!settings?.transport.unixSocket}
                    onCheckedChange={(v) => setTransport('unixSocket', v === true)}
                  />
                  Unix socket（推荐的桥接通道）
                </label>
                <label className="flex items-center gap-2">
                  <Checkbox
                    checked={!!settings?.transport.tcp}
                    onCheckedChange={(v) => setTransport('tcp', v === true)}
                  />
                  TCP 回环端点（需 token）
                </label>
              </div>

              <div className="flex flex-wrap items-center gap-3">
                <span className="text-xs text-muted-foreground w-32">固定端口（0=随机）</span>
                <Input
                  className="h-8 w-24 sm:w-32"
                  value={portDraft}
                  onChange={(e) => setPortDraft(e.target.value)}
                  placeholder="0"
                />
                <span className="text-xs text-muted-foreground w-32">审批超时（秒）</span>
                <Input
                  className="h-8 w-20 sm:w-24"
                  value={timeoutDraft}
                  onChange={(e) => setTimeoutDraft(e.target.value)}
                />
                <Button size="sm" className="gap-1.5" onClick={saveAdvanced}>
                  <Save className="h-3.5 w-3.5" />
                  保存
                </Button>
              </div>

              <div className="text-xs text-muted-foreground break-all">
                设置文件：<code>{status.settingsPath || '-'}</code>
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

function AuditList({ entries }: { entries: AuditEntry[] }): React.JSX.Element {
  if (!entries.length) {
    return <div className="py-6 text-center text-xs text-muted-foreground">暂无记录</div>
  }
  return (
    <div className="max-h-72 min-w-0 space-y-1 overflow-y-auto overflow-x-hidden">
      {entries.map((e, i) => (
        <div
          key={`${e.time}-${e.tool}-${i}`}
          className="flex min-w-0 items-start gap-2 rounded-md px-2 py-1.5 text-xs hover:bg-accent/40 sm:gap-3"
        >
          <span className="w-12 shrink-0 text-muted-foreground sm:w-16">{formatTime(e.time)}</span>
          <code className="w-24 shrink-0 truncate sm:w-40" title={e.tool}>{e.tool}</code>
          <span className="hidden w-24 shrink-0 truncate text-muted-foreground sm:inline">{e.client || '-'}</span>
          <span className="shrink-0">{decisionBadge(e.decision)}</span>
          <span className="w-12 shrink-0 text-right text-muted-foreground sm:w-14">{e.durationMs}ms</span>
          <span className="min-w-0 flex-1 truncate text-muted-foreground" title={e.summary || e.args}>
            {e.summary || e.args}
          </span>
        </div>
      ))}
    </div>
  )
}
