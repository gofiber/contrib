package fiberzap

import (
	"context"
	"fmt"
	"io"
	"os"

	fiberlog "github.com/gofiber/fiber/v2/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var _ fiberlog.AllLogger = (*LoggerConfig)(nil)

type LoggerConfig struct {
	CoreConfigs []CoreConfig
	ZapOptions  []zap.Option
	ExtraKeys   []string

	// SetLogger sets *zap.Logger for fiberlog, if set, ZapOptions, CoreConfigs, SetLevel, SetOutput will be ignored
	//
	// Optional. Default: nil
	SetLogger *zap.Logger

	logger *zap.Logger
}

func (l *LoggerConfig) WithContext(ctx context.Context) fiberlog.CommonLogger {
	log := l.logger.Sugar()
	if len(l.ExtraKeys) > 0 {
		for _, k := range l.ExtraKeys {
			value := ctx.Value(k)
			log = log.With(k, value)
		}
	}
	return l
}

type CoreConfig struct {
	Encoder      zapcore.Encoder
	WriteSyncer  zapcore.WriteSyncer
	LevelEncoder zapcore.LevelEnabler
}

// LoggerConfigDefault is the default config
var LoggerConfigDefault = LoggerConfig{
	CoreConfigs: []CoreConfig{
		{
			Encoder:      zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			WriteSyncer:  zapcore.AddSync(os.Stdout),
			LevelEncoder: zap.NewAtomicLevelAt(zap.InfoLevel),
		},
	},
	ZapOptions: []zap.Option{},
	ExtraKeys:  []string{},
}

func loggerConfigDefault(config ...LoggerConfig) LoggerConfig {
	// Return default config if nothing provided
	if len(config) < 1 {
		return LoggerConfigDefault
	}

	// Override default config
	cfg := config[0]

	if cfg.CoreConfigs == nil {
		cfg.CoreConfigs = LoggerConfigDefault.CoreConfigs
	}

	if cfg.ZapOptions == nil {
		cfg.ZapOptions = LoggerConfigDefault.ZapOptions
	}

	if cfg.ExtraKeys == nil {
		cfg.ExtraKeys = LoggerConfigDefault.ExtraKeys
	}

	if cfg.SetLogger != nil {
		cfg.logger = cfg.SetLogger
	}

	// Remove duplicated extraKeys
	for _, k := range cfg.ExtraKeys {
		if !contains(k, cfg.ExtraKeys) {
			cfg.ExtraKeys = append(cfg.ExtraKeys, k)
		}
	}

	return cfg
}

// NewLogger creates a new zap logger adapter for fiberlog
func NewLogger(config ...LoggerConfig) *LoggerConfig {
	cfg := loggerConfigDefault(config...)

	// Return logger if already exists
	if cfg.SetLogger != nil {
		return &cfg
	}

	zapCores := make([]zapcore.Core, len(cfg.CoreConfigs))
	for i, coreConfig := range cfg.CoreConfigs {
		zapCores[i] = zapcore.NewCore(coreConfig.Encoder, coreConfig.WriteSyncer, coreConfig.LevelEncoder)
	}

	core := zapcore.NewTee(zapCores...)
	logger := zap.New(core, cfg.ZapOptions...)
	cfg.logger = logger

	return &cfg
}

// SetOutput sets the output destination for the logger.
func (l *LoggerConfig) SetOutput(w io.Writer) {
	if l.SetLogger != nil {
		fiberlog.Warn("SetLevel is ignored when SetLogger is set")
		return
	}
	l.CoreConfigs[0].WriteSyncer = zapcore.AddSync(w)
	zapCores := make([]zapcore.Core, len(l.CoreConfigs))
	for i, coreConfig := range l.CoreConfigs {
		zapCores[i] = zapcore.NewCore(coreConfig.Encoder, coreConfig.WriteSyncer, coreConfig.LevelEncoder)
	}

	core := zapcore.NewTee(zapCores...)
	logger := zap.New(core, l.ZapOptions...)

	l.logger = logger
}

func (l *LoggerConfig) SetLevel(lv fiberlog.Level) {
	if l.SetLogger != nil {
		fiberlog.Warn("SetLevel is ignored when SetLogger is set")
		return
	}
	var level zapcore.Level
	switch lv {
	case fiberlog.LevelTrace, fiberlog.LevelDebug:
		level = zap.DebugLevel
	case fiberlog.LevelInfo:
		level = zap.InfoLevel
	case fiberlog.LevelWarn:
		level = zap.WarnLevel
	case fiberlog.LevelError:
		level = zap.ErrorLevel
	case fiberlog.LevelFatal:
		level = zap.FatalLevel
	case fiberlog.LevelPanic:
		level = zap.PanicLevel
	default:
		level = zap.WarnLevel
	}

	l.CoreConfigs[0].LevelEncoder = level
	zapCores := make([]zapcore.Core, len(l.CoreConfigs))
	for i, coreConfig := range l.CoreConfigs {
		zapCores[i] = zapcore.NewCore(coreConfig.Encoder, coreConfig.WriteSyncer, coreConfig.LevelEncoder)
	}

	core := zapcore.NewTee(zapCores...)
	l.logger = zap.New(core, l.ZapOptions...)
}

func (l *LoggerConfig) logf(level fiberlog.Level, format string, kvs ...interface{}) {
	logger := l.logger.Sugar()
	switch level {
	case fiberlog.LevelTrace, fiberlog.LevelDebug:
		logger.Debugf(format, kvs...)
	case fiberlog.LevelInfo:
		logger.Infof(format, kvs...)
	case fiberlog.LevelWarn:
		logger.Warnf(format, kvs...)
	case fiberlog.LevelError:
		logger.Errorf(format, kvs...)
	case fiberlog.LevelFatal:
		logger.Fatalf(format, kvs...)
	default:
		logger.Warnf(format, kvs...)
	}
}

func (l *LoggerConfig) ctxLogf(level fiberlog.Level, ctx context.Context, format string, kvs ...interface{}) {
	log := l.logger.Sugar()
	if len(l.ExtraKeys) > 0 {
		for _, k := range l.ExtraKeys {
			log = log.With(k, ctx.Value(k))
		}
	}
	switch level {
	case fiberlog.LevelDebug, fiberlog.LevelTrace:
		log.Debugf(format, kvs...)
	case fiberlog.LevelInfo:
		log.Infof(format, kvs...)
	case fiberlog.LevelWarn:
		log.Warnf(format, kvs...)
	case fiberlog.LevelError:
		log.Errorf(format, kvs...)
	case fiberlog.LevelFatal:
		log.Fatalf(format, kvs...)
	case fiberlog.LevelPanic:
		log.Panicf(format, kvs...)
	default:
		log.Warnf(format, kvs...)
	}
}

func (l *LoggerConfig) Trace(v ...interface{}) {
	l.log(fiberlog.LevelTrace, v...)
}

func (l *LoggerConfig) Debug(v ...interface{}) {
	l.log(fiberlog.LevelDebug, v...)
}

func (l *LoggerConfig) Info(v ...interface{}) {
	l.log(fiberlog.LevelInfo, v...)
}

func (l *LoggerConfig) Warn(v ...interface{}) {
	l.log(fiberlog.LevelWarn, v...)
}

func (l *LoggerConfig) Error(v ...interface{}) {
	l.log(fiberlog.LevelError, v...)
}

func (l *LoggerConfig) Fatal(v ...interface{}) {
	l.log(fiberlog.LevelFatal, v...)
}

func (l *LoggerConfig) Panic(v ...interface{}) {
	l.log(fiberlog.LevelPanic, v...)
}

func (l *LoggerConfig) Tracef(format string, v ...interface{}) {
	l.logf(fiberlog.LevelTrace, format, v...)
}

func (l *LoggerConfig) Debugf(format string, v ...interface{}) {
	l.logf(fiberlog.LevelDebug, format, v...)
}

func (l *LoggerConfig) Infof(format string, v ...interface{}) {
	l.logf(fiberlog.LevelInfo, format, v...)
}

func (l *LoggerConfig) Warnf(format string, v ...interface{}) {
	l.logf(fiberlog.LevelWarn, format, v...)
}

func (l *LoggerConfig) Errorf(format string, v ...interface{}) {
	l.logf(fiberlog.LevelError, format, v...)
}

func (l *LoggerConfig) Fatalf(format string, v ...interface{}) {
	l.logf(fiberlog.LevelFatal, format, v...)
}

func (l *LoggerConfig) Panicf(format string, v ...interface{}) {
	l.logf(fiberlog.LevelPanic, format, v...)
}

func (l *LoggerConfig) CtxTracef(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelTrace, ctx, format, v...)
}

func (l *LoggerConfig) CtxDebugf(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelDebug, ctx, format, v...)
}

func (l *LoggerConfig) CtxInfof(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelInfo, ctx, format, v...)
}

func (l *LoggerConfig) CtxWarnf(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelWarn, ctx, format, v...)
}

func (l *LoggerConfig) CtxErrorf(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelError, ctx, format, v...)
}

func (l *LoggerConfig) CtxFatalf(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelFatal, ctx, format, v...)
}

func (l *LoggerConfig) CtxPanicf(ctx context.Context, format string, v ...interface{}) {
	l.ctxLogf(fiberlog.LevelPanic, ctx, format, v...)
}

func (l *LoggerConfig) Tracew(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelTrace, msg, keysAndValues...)
}

func (l *LoggerConfig) Debugw(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelDebug, msg, keysAndValues...)
}

func (l *LoggerConfig) Infow(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelInfo, msg, keysAndValues...)
}

func (l *LoggerConfig) Warnw(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelWarn, msg, keysAndValues...)
}

func (l *LoggerConfig) Errorw(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelError, msg, keysAndValues...)
}

func (l *LoggerConfig) Fatalw(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelFatal, msg, keysAndValues...)
}

func (l *LoggerConfig) Panicw(msg string, keysAndValues ...interface{}) {
	l.logw(fiberlog.LevelPanic, msg, keysAndValues...)
}

func (l *LoggerConfig) log(level fiberlog.Level, kvs ...interface{}) {
	sugar := l.logger.Sugar()
	switch level {
	case fiberlog.LevelTrace, fiberlog.LevelDebug:
		sugar.Debug(kvs...)
	case fiberlog.LevelInfo:
		sugar.Info(kvs...)
	case fiberlog.LevelWarn:
		sugar.Warn(kvs...)
	case fiberlog.LevelError:
		sugar.Error(kvs...)
	case fiberlog.LevelFatal:
		sugar.Fatal(kvs...)
	case fiberlog.LevelPanic:
		sugar.Panic(kvs...)
	default:
		sugar.Warn(kvs...)
	}
}

func (l *LoggerConfig) logw(level fiberlog.Level, msg string, keyvals ...interface{}) {
	keylen := len(keyvals)
	if keylen == 0 || keylen%2-1 != 0 {
		l.Logger().Warn(fmt.Sprint("Keyvalues must appear in pairs: ", keyvals))
		return
	}
	data := make([]zap.Field, 0, (keylen/2)+1)
	for i := 0; i < keylen; i += 2 {
		data = append(data, zap.Any(fmt.Sprint(keyvals[i]), keyvals[i+1]))
	}
	switch level {
	case fiberlog.LevelTrace, fiberlog.LevelDebug:
		l.Logger().Debug(msg, data...)
	case fiberlog.LevelInfo:
		l.Logger().Info(msg, data...)
	case fiberlog.LevelWarn:
		l.Logger().Warn(msg, data...)
	case fiberlog.LevelError:
		l.Logger().Error(msg, data...)
	case fiberlog.LevelFatal:
		l.Logger().Fatal(msg, data...)
	default:
		l.Logger().Warn(msg, data...)
	}
}

func (l *LoggerConfig) Log(level fiberlog.Level, keyvals ...interface{}) error {
	keylen := len(keyvals)
	if keylen == 0 || keylen%2 != 0 {
		l.Logger().Warn(fmt.Sprint("Keyvalues must appear in pairs: ", keyvals))
		return nil
	}

	data := make([]zap.Field, 0, (keylen/2)+1)
	for i := 0; i < keylen; i += 2 {
		data = append(data, zap.Any(fmt.Sprint(keyvals[i]), keyvals[i+1]))
	}
	switch level {
	case fiberlog.LevelTrace, fiberlog.LevelDebug:
		l.Logger().Debug("", data...)
	case fiberlog.LevelInfo:
		l.Logger().Info("", data...)
	case fiberlog.LevelWarn:
		l.Logger().Warn("", data...)
	case fiberlog.LevelError:
		l.Logger().Error("", data...)
	case fiberlog.LevelFatal:
		l.Logger().Fatal("", data...)
	default:
		l.Logger().Warn("", data...)
	}
	return nil
}

// Sync flushes any buffered log entries.
func (l *LoggerConfig) Sync() error {
	return l.logger.Sync()
}

// Logger returns the underlying *zap.Logger when not using SetLogger
// if you want to customize the field, you can use Logger to manipulate the native zap
func (l *LoggerConfig) Logger() *zap.Logger {
	return l.logger
}
