package logging

import (
	"context"

	"github.com/go-logr/logr"
	uberzap "go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// NewTestLogger creates a new Zap logger using the dev mode.
func NewTestLogger() logr.Logger {
	return zap.New(zap.UseDevMode(true), zap.RawZapOpts(uberzap.AddCaller()))
}

// NewTestLoggerIntoContext creates a new Zap logger using the dev mode and inserts it into the given context.
func NewTestLoggerIntoContext(ctx context.Context) context.Context {
	return log.IntoContext(ctx, zap.New(zap.UseDevMode(true), zap.RawZapOpts(uberzap.AddCaller())))
}
