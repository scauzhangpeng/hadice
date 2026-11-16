import React, { useEffect, useState } from 'react'
import { ShieldAlert } from 'lucide-react'
import { toast } from 'sonner'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { EventsOn as EventsOnRuntime } from '../../../wailsjs/runtime/runtime'
import * as MCPService from '../../../bindings/Hadice/backend/mcpservice'

interface ApprovalRequest {
  id: string
  tool: string
  client?: string
  args?: string
  createdAt: string
  timeoutSec: number
}

/**
 * 高危工具审批弹窗。
 *
 * 由 App 后端通过 `mcp:approval` 事件触发：MCP 客户端（WorkBuddy / Codex…）
 * 调用 install_app / uninstall_app / clear_app_data 时，服务端会挂起这次调用，
 * 并在这里等用户点“允许”。关掉弹窗/超时都按拒绝处理。
 *
 * 挂在 App 主窗口，任何页面都能弹出，不依赖用户停留在 MCP 设置页。
 */
export function McpApprovalDialog(): React.JSX.Element {
  const [req, setReq] = useState<ApprovalRequest | null>(null)
  const [remember, setRemember] = useState(false)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    const unlisten = EventsOnRuntime('mcp:approval', (payload: ApprovalRequest) => {
      setRemember(false)
      setReq(payload)
    })
    return () => {
      if (typeof unlisten === 'function') unlisten()
    }
  }, [])

  const resolve = async (allow: boolean) => {
    if (!req || busy) return
    setBusy(true)
    try {
      await MCPService.ResolveMCPApproval(req.id, allow, remember)
      toast[allow ? 'success' : 'message'](
        allow ? `已允许 ${req.tool}` : `已拒绝 ${req.tool}`
      )
    } catch (err) {
      toast.error(`回传审批结果失败：${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setBusy(false)
      setReq(null)
    }
  }

  return (
    <Dialog open={!!req} onOpenChange={(open) => !open && resolve(false)}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <ShieldAlert className="h-5 w-5 text-yellow-500" />
            高危工具调用需要确认
          </DialogTitle>
          <DialogDescription>
            MCP 客户端请求在设备上执行一个会改变应用状态的操作，请确认是你发起的。
          </DialogDescription>
        </DialogHeader>

        {req && (
          <div className="space-y-3 text-sm">
            <div className="grid grid-cols-[80px_1fr] gap-y-2">
              <span className="text-muted-foreground">工具</span>
              <span className="font-mono">{req.tool}</span>
              <span className="text-muted-foreground">调用方</span>
              <span>{req.client || '未知'}</span>
              <span className="text-muted-foreground">超时</span>
              <span>
                {req.timeoutSec} 秒后自动拒绝
              </span>
            </div>
            <div>
              <div className="text-muted-foreground mb-1">参数</div>
              <pre className="max-h-40 overflow-auto rounded-md bg-muted/60 p-2 text-xs whitespace-pre-wrap break-all">
                {req.args || '（无参数）'}
              </pre>
            </div>
          </div>
        )}

        <DialogFooter className="flex items-center justify-between gap-2 sm:justify-between">
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Checkbox
              checked={remember}
              onCheckedChange={(v) => setRemember(v === true)}
            />
            记住允许 {req?.tool}（持久保存，可在设置页清除）
          </label>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" disabled={busy} onClick={() => resolve(false)}>
              拒绝
            </Button>
            <Button size="sm" disabled={busy} onClick={() => resolve(true)}>
              允许执行
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
