package clierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestCode(t *testing.T) {
	base := errors.New("boom")
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantOK   bool
	}{
		{name: "nil", err: nil, wantCode: 0, wantOK: false},
		{name: "plain error", err: base, wantCode: 1, wantOK: false},
		{name: "wrapped with exit", err: WithExit(base, 3), wantCode: 3, wantOK: true},
		{name: "exit under fmt wrap", err: fmt.Errorf("context: %w", WithExit(base, 4)), wantCode: 4, wantOK: true},
		{name: "outermost wins", err: WithExit(WithExit(base, 2), 5), wantCode: 5, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := Code(tc.err)
			if code != tc.wantCode || ok != tc.wantOK {
				t.Errorf("Code() = (%d, %t), want (%d, %t)", code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}

func TestWithExitNilIsNil(t *testing.T) {
	if err := WithExit(nil, 3); err != nil {
		t.Errorf("WithExit(nil) = %v, want nil", err)
	}
}

func TestWithExitPreservesChain(t *testing.T) {
	base := errors.New("boom")
	err := WithExit(base, 3)
	if !errors.Is(err, base) {
		t.Error("errors.Is should see the wrapped cause")
	}
	if err.Error() != "boom" {
		t.Errorf("Error() = %q, want the cause's message unchanged", err.Error())
	}
}
