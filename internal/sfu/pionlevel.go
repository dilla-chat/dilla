package sfu

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/protocol/logger"
)

// pionLevel is the lowest level pion's loggers are asked to write at. The bridge drops pion's
// records below ERROR anyway (NewLogBridge); gating here as well keeps pion's Debugf from running.
const pionLevel = zapcore.WarnLevel

// pionGated is the LiveKit logger with the one capability that makes pionlogger.NewLoggerFactory
// level-gate pion's loggers. For a plain logger the factory builds an adapter that runs
// fmt.Sprintf on every Debugf before asking any level, so pion's debug lines, such as the
// onTrack one that prints the receiver with %v, read the receiver by reflection on whichever
// goroutine logs while SetLocalDescription writes it: a data race (go test -race, CI go-3). A
// logger that is a logger.ZapLogger gets the adapter that asks ComponentLeveler first and formats
// nothing below pionLevel. Only ComponentLeveler is ever read; the zap parts are inert.
//
// Every derived logger stays a pionGated, since LiveKit derives its transport loggers with
// WithValues and WithName before it builds pion's factory from them.
type pionGated struct{ logger.Logger }

func (g pionGated) WithValues(kv ...any) logger.Logger { return pionGated{g.Logger.WithValues(kv...)} }
func (g pionGated) WithName(n string) logger.Logger    { return pionGated{g.Logger.WithName(n)} }
func (g pionGated) WithComponent(c string) logger.Logger {
	return pionGated{g.Logger.WithComponent(c)}
}
func (g pionGated) WithCallDepth(d int) logger.Logger { return pionGated{g.Logger.WithCallDepth(d)} }
func (g pionGated) WithItemSampler() logger.Logger    { return pionGated{g.Logger.WithItemSampler()} }
func (g pionGated) WithoutSampler() logger.Logger     { return pionGated{g.Logger.WithoutSampler()} }
func (g pionGated) WithDeferredValues() (logger.Logger, logger.DeferredFieldResolver) {
	l, r := g.Logger.WithDeferredValues()
	return pionGated{l}, r
}

// ToZap is never used: ToSlogHandler is the only reader and nothing here calls it.
func (pionGated) ToZap() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func (pionGated) ComponentLeveler() logger.ZapComponentLeveler { return pionLeveler{} }
func (pionGated) Leveler() *logger.ComponentLeveler            { return nil }
func (g pionGated) WithComponentLeveler(*logger.ComponentLeveler) logger.Logger {
	return g
}

type pionLeveler struct{}

func (pionLeveler) ComponentLevel(string) zapcore.LevelEnabler { return pionLevel }
