package googlegroup

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zaptest.NewLogger(t)
}
