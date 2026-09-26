package ateclient

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RetryPolicy struct {
	Attempts int
	Initial  time.Duration
	Max      time.Duration
}

var defaultRetry = RetryPolicy{Attempts: 5, Initial: 100 * time.Millisecond, Max: 2 * time.Second}

// RetryAborted retries calls that fail with codes.Aborted, which ateapi returns
// when another operation holds the object's lease. Update calls are not
// retried: there Aborted means a uid/version conflict and the caller must re-read.
func RetryAborted(p RetryPolicy) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if strings.Contains(method, "/Update") {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		delay := p.Initial
		var err error
		for attempt := 1; ; attempt++ {
			err = invoker(ctx, method, req, reply, cc, opts...)
			if status.Code(err) != codes.Aborted || attempt >= p.Attempts {
				return err
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return err
			case <-t.C:
			}
			delay = min(delay*2, p.Max)
		}
	}
}
