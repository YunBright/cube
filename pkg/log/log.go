// Package log 提供结构化日志。
//
// 设计要点:
//   - 所有 dapr app 用同一个 Logger,保证字段一致
//   - 默认走 dapr logging sidecar(dapr 通过 --log-as-json 转发)
//   - 字段命名 snake_case,便于 ELK / Loki 聚合
package log

import (
	"context"
	"log/slog"
	"os"
)

// Logger 包装 slog.Logger,提供项目惯用字段。
type Logger struct {
	*slog.Logger
	appID string
}

// New 构造 Logger。
func New(appID string) *Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &Logger{
		Logger: slog.New(h).With("app_id", appID),
		appID:  appID,
	}
}

// WithComponent 返回带 component 字段的子 logger。
func (l *Logger) WithComponent(c string) *Logger {
	return &Logger{Logger: l.Logger.With("component", c), appID: l.appID}
}

// WithPrincipal 返回带 principal 字段的子 logger(用于审计)。
func (l *Logger) WithPrincipal(p string) *Logger {
	return &Logger{Logger: l.Logger.With("principal", p), appID: l.appID}
}

// Ctx 携带 ctx 的便捷方法。
func (l *Logger) Ctx(ctx context.Context) *Logger {
	_ = ctx
	return l
}