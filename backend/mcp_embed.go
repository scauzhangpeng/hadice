package backend

// App 内嵌 MCP 服务：Hadice App 运行即对外提供 MCP 端点。
//
// 启动两种本地传输并把端点写入 discovery 文件，供 hadice-mcp 桥接进程发现：
//   - Unix domain socket（默认）：文件权限 0600 即鉴权，浏览器无法访问；
//   - TCP 127.0.0.1 随机端口：给只支持 URL 的客户端，靠 token + Host/Origin 校验。
//
// App 进程退出时随进程消亡；discovery 文件异常退出残留由 PID 存活检查兜底。
//
// 说明：内嵌 Runtime 复用 backend 全局的 deviceManager（与 GUI 同一实例），
// 设备缓存与状态完全共享；桥接出去的工具能力因此与 GUI 对齐。
//
// 纯新增设计：不改动任何既有文件，由包 init() 自启动，无需在 backend.Run()
// 中接线。deviceManager 在 backend/device.go 的 init() 中初始化（文件序先于
// 本文件），init 时已就绪；此处仍做短时轮询兜底，避免依赖文件序假设。

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"Hadice/backend/mcpserver"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 审批事件名：前端监听后用模态框展示，用户点允许/拒绝再回调 MCPService。
const mcpApprovalEvent = "mcp:approval"

var (
	mcpMu     sync.Mutex
	mcpRT     *mcpserver.Runtime
	mcpStop   func()
	mcpErrMsg string
)

func init() {
	if os.Getenv("HADICE_MCP_DISABLED") == "1" {
		log.Println("[MCP] HADICE_MCP_DISABLED=1, skip embedded MCP server")
		return
	}

	go startEmbeddedMCP()
}

// startEmbeddedMCP 等待 deviceManager 就绪后启动内嵌 MCP 服务（异步）。
func startEmbeddedMCP() {
	for i := 0; i < 100 && deviceManager == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if deviceManager == nil {
		// MCP 属增强能力，等待失败不影响 App 主流程，仅记录日志。
		log.Println("[MCP] deviceManager not ready within 1s, skip embedded MCP server")
		return
	}

	rt := mcpserver.NewRuntime(deviceManager)
	rt.Approver().SetNotifier(notifyMCPApproval)

	mcpMu.Lock()
	mcpRT = rt
	mcpMu.Unlock()

	if !rt.Store().Get().Enabled {
		log.Println("[MCP] disabled in settings, endpoints not started")
	} else if err := startMCPEndpoints(rt); err != nil {
		mcpMu.Lock()
		mcpErrMsg = err.Error()
		mcpMu.Unlock()
		log.Printf("[MCP] embedded server start failed: %v", err)
	}

	// 设置页切换"启用/停用"时立即生效，不需要重启 App。
	rt.Store().Subscribe(func(s mcpserver.Settings) {
		mcpMu.Lock()
		running := mcpStop != nil
		mcpMu.Unlock()

		switch {
		case s.Enabled && !running:
			if err := startMCPEndpoints(rt); err != nil {
				log.Printf("[MCP] restart after enable failed: %v", err)
			}
		case !s.Enabled && running:
			stopMCPEndpoints(rt)
			log.Println("[MCP] disabled in settings, endpoints closed")
		}
	})
}

// startMCPEndpoints 按设置启动监听并写入 discovery 文件。
func startMCPEndpoints(rt *mcpserver.Runtime) error {
	s := rt.Store().Get()
	opt := mcpserver.ServeOptions{Token: mcpserver.NewToken()}
	if s.Transport.UnixSocket {
		opt.SocketPath = mcpserver.DefaultSocketPath()
	}
	if s.Transport.TCP {
		opt.TCPAddr = "127.0.0.1:0"
		if p := s.Transport.FixedPort; p > 0 {
			opt.TCPAddr = fmt.Sprintf("127.0.0.1:%d", p)
		}
	}

	eps, closeFn, err := mcpserver.Serve(mcpserver.New(rt), opt)
	if err != nil {
		return err
	}

	transport := mcpserver.TransportUnix
	if eps.Socket == "" {
		transport = mcpserver.TransportTCP
	}
	info := mcpserver.EndpointInfo{
		Transport: transport,
		Endpoint:  eps.TCP,
		Socket:    eps.Socket,
		Token:     eps.Token,
		PID:       os.Getpid(),
		Version:   mcpserver.ServerVersion,
		Build:     mcpserver.BuildInfo(),
	}
	if werr := mcpserver.WriteEndpointFile(info); werr != nil {
		// 写不进去（沙箱/权限）时端点仍然可用，只是桥接进程发现不了，必须留日志。
		log.Printf("[MCP] write discovery file failed: %v", werr)
	}

	mcpMu.Lock()
	mcpStop = closeFn
	mcpErrMsg = ""
	mcpMu.Unlock()
	rt.SetEndpoints(eps, closeFn)

	log.Printf("[MCP] embedded server ready: socket=%q tcp=%q (discovery written)", eps.Socket, eps.TCP)
	return nil
}

func stopMCPEndpoints(rt *mcpserver.Runtime) {
	mcpMu.Lock()
	stop := mcpStop
	mcpStop = nil
	mcpMu.Unlock()
	if stop != nil {
		stop()
	}
	rt.SetEndpoints(mcpserver.Endpoints{}, nil)
	_ = mcpserver.RemoveEndpointFile()
}

// notifyMCPApproval 把审批请求推给前端。返回 false 表示 UI 不可用（按拒绝处理）。
func notifyMCPApproval(req mcpserver.ApprovalRequest) bool {
	app := application.Get()
	if app == nil {
		return false
	}
	app.Event.Emit(mcpApprovalEvent, req)
	return true
}

// mcpRuntime 返回内嵌 MCP 运行时（GUI 服务读取状态用）。
func mcpRuntime() (*mcpserver.Runtime, error) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if mcpRT == nil {
		if mcpErrMsg != "" {
			return nil, fmt.Errorf("MCP 未启动：%s", mcpErrMsg)
		}
		return nil, fmt.Errorf("MCP 未启动（App 刚启动时可能仍在初始化，请稍后重试）")
	}
	return mcpRT, nil
}
