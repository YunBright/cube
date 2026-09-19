// Package daprmanager 管理 dapr cube app 的本地进程(dev 模式)。
//
// 设计要点(P0-4):
//   - MVP:本机进程 + dapr run wrapper
//   - 生产 hosted:需要 k8s client 重启 Pod,留 TODO
//   - cube-compiler 编译完后调 Reload → 进程退出 → 重启
package daprmanager

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/YunBright/cube/pkg/log"
)

// Config 是进程管理器配置。
type Config struct {
	OutputDir string // 编译产物目录
	LocalRepo string // 本地 git 仓库路径
}

// Manager 持有进程表。
type Manager struct {
	cfg    Config
	logger  *log.Logger
	procs  map[string]*proc // app_id → proc
	mu     sync.Mutex
}

type proc struct {
	appID string
	cmd   *exec.Cmd
}

// New 构造 Manager。
func New(cfg Config, lg *log.Logger) *Manager {
	return &Manager{
		cfg:    cfg,
		logger: lg,
		procs:  map[string]*proc{},
	}
}

// Build 编译一个 cube app(app_id 对应 cmd 路径)。
func (m *Manager) Build(ctx context.Context, appID, cmdPath string) error {
	// TODO: go build -o <OutputDir>/<app_id> <cmdPath>
	// 简化:假定已编译,只返回 nil
	_ = ctx
	_ = cmdPath
	m.logger.Info("built (stub)", "app_id", appID)
	return nil
}

// Reload 重启一个 cube app:kill 旧进程 + start 新进程。
func (m *Manager) Reload(ctx context.Context, appID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p, ok := m.procs[appID]; ok {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		delete(m.procs, appID)
	}
	return m.start(ctx, appID)
}

func (m *Manager) start(ctx context.Context, appID string) error {
	// TODO: exec.Command("dapr", "run", "--app-id", appID, "--", "<binary>")
	// 简化:返回成功,真实启动留给 dev 环境
	m.procs[appID] = &proc{appID: appID}
	m.logger.Info("started (stub)", "app_id", appID)
	return nil
}

// StopAll 关闭所有进程(graceful shutdown)。
func (m *Manager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for id, p := range m.procs {
		if p.cmd != nil && p.cmd.Process != nil {
			if err := p.cmd.Process.Kill(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
			}
		}
	}
	m.procs = map[string]*proc{}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}