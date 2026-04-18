package tracing

import (
	"context"
	"fmt"
)

type ctxKey int

const (
	traceKey ctxKey = 1
)

type Trace struct {
	Id              string `json:"trace_id"`
	RequestMethod   string `json:"request_method,omitempty"`
	RequestEndpoint string `json:"request_endpoint,omitempty"`
}

func WithTrace(ctx context.Context, trace Trace) context.Context {
	return context.WithValue(ctx, traceKey, trace)
}

func GetTrace(ctx context.Context) (Trace, error) {
	trace, ok := ctx.Value(traceKey).(Trace)
	if !ok {
		return Trace{Id: "000000-000000-000000"}, fmt.Errorf("trace-id not found in context")
	}
	return trace, nil
}
