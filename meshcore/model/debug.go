package model

import (
	"context"
	"io"
)

// debugCtxKey carries an optional per-invocation debug writer. It lives in the model package so
// every adapter family (shell, acpagent, …) reads the SAME key — an app enables diagnostics once
// (behind a --debug flag) and any adapter it drives emits to the same writer.
type debugCtxKey struct{}

// WithDebug returns a context that makes adapters emit a per-invocation diagnostic to w (resolved
// binary/argv or ACP flow, exit/stop, extracted identity + evidence, captured stderr/stdout). Apps
// wire this to os.Stderr behind --debug; it is off by default. A nil writer is a no-op.
func WithDebug(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, debugCtxKey{}, w)
}

// DebugWriter returns the debug writer set by WithDebug, or nil.
func DebugWriter(ctx context.Context) io.Writer {
	w, _ := ctx.Value(debugCtxKey{}).(io.Writer)
	return w
}
