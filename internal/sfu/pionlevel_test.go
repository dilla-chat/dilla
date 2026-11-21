package sfu

import (
	"testing"

	"go.uber.org/zap"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"
)

// formatted counts how often %v-formatting reaches String.
type formatted struct{ n *int }

func (f formatted) String() string { *f.n++; return "x" }

// TestPionDebugNeverFormats pins the CI go-3 fix: pion's factory, built from the logger LiveKit
// is given (and from loggers derived from it), is level-gated, so Debugf and Infof format nothing
// while Warnf and Errorf still do.
func TestPionDebugNeverFormats(t *testing.T) {
	var l logger.Logger = pionGated{logger.GetDiscardLogger()}
	l = l.WithValues("a", 1).WithName("transport").WithComponent("x").WithCallDepth(1)
	f := pionlogger.NewLoggerFactory(l)
	log := f.NewLogger("pc")

	n := 0
	log.Tracef("%v", formatted{&n})
	log.Debugf("%v", formatted{&n})
	log.Infof("%v", formatted{&n})
	if n != 0 {
		t.Fatalf("pion's trace/debug/info lines were formatted %d times; want 0", n)
	}
	log.Warnf("%v", formatted{&n})
	log.Errorf("%v", formatted{&n})
	if n != 2 {
		t.Fatalf("pion's warn/error lines formatted %d times; want 2", n)
	}
}

func TestInstalledLiveKitLoggerGatesPion(t *testing.T) {
	zl, ok := logger.GetLogger().(logger.ZapLogger)
	if !ok {
		t.Fatalf("LiveKit's logger is %T, not a ZapLogger: pion's factory would format every debug line", logger.GetLogger())
	}
	if zl.ComponentLeveler().ComponentLevel("pion.pc").Enabled(zap.DebugLevel) {
		t.Fatal("pion's debug level is enabled")
	}
}
