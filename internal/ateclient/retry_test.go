package ateclient

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRetryAborted(t *testing.T) {
	p := RetryPolicy{Attempts: 3, Initial: time.Millisecond, Max: time.Millisecond}
	tests := []struct {
		name      string
		method    string
		errs      []codes.Code
		wantCode  codes.Code
		wantCalls int
	}{
		{name: "success", errs: []codes.Code{codes.OK}, wantCode: codes.OK, wantCalls: 1},
		{name: "aborted then ok", errs: []codes.Code{codes.Aborted, codes.OK}, wantCode: codes.OK, wantCalls: 2},
		{name: "gives up", errs: []codes.Code{codes.Aborted, codes.Aborted, codes.Aborted, codes.OK}, wantCode: codes.Aborted, wantCalls: 3},
		{name: "no retry on updates", method: "/ateapi.Control/UpdateActor", errs: []codes.Code{codes.Aborted, codes.OK}, wantCode: codes.Aborted, wantCalls: 1},
		{name: "no retry on other codes", errs: []codes.Code{codes.FailedPrecondition, codes.OK}, wantCode: codes.FailedPrecondition, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
				c := tt.errs[calls]
				calls++
				if c == codes.OK {
					return nil
				}
				return status.Error(c, "x")
			}
			err := RetryAborted(p)(context.Background(), method(tt.method), nil, nil, nil, invoker)
			if status.Code(err) != tt.wantCode || calls != tt.wantCalls {
				t.Fatalf("code=%v calls=%d, want %v/%d", status.Code(err), calls, tt.wantCode, tt.wantCalls)
			}
		})
	}
}

func method(m string) string {
	if m == "" {
		return "/ateapi.Control/SuspendActor"
	}
	return m
}
