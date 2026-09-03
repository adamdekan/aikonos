package audit

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// TestRecordEmitFailure_NoLogger verifies RecordEmitFailure does not panic when
// the logger is nil (some call sites may not have one in scope).
func TestRecordEmitFailure_NoLogger(t *testing.T) {
	RecordEmitFailure(context.Background(), nil, errors.New("emit failed"), "aikonos.broker.task.created")
}

// TestRecordEmitFailure_WithLogger verifies RecordEmitFailure does not panic
// when a real logger is provided.
func TestRecordEmitFailure_WithLogger(t *testing.T) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}
	RecordEmitFailure(context.Background(), logger, errors.New("emit failed"), "aikonos.broker.tool.invoked")
}

// TestRecordEmitFailure_NilError verifies RecordEmitFailure does not panic when
// called with a nil error (defensive: callers should only call on non-nil, but guard anyway).
func TestRecordEmitFailure_NilError(t *testing.T) {
	RecordEmitFailure(context.Background(), nil, nil, "aikonos.broker.plan.validated")
}
