package fiberzap

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3/log"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// testEncoderConfig encoder config for testing, copy from zap
func testEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "level",
		NameKey:        "name",
		TimeKey:        "ts",
		CallerKey:      "caller",
		FunctionKey:    "func",
		StacktraceKey:  "stacktrace",
		LineEnding:     "\n",
		EncodeTime:     zapcore.EpochTimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// humanEncoderConfig copy from zap
func humanEncoderConfig() zapcore.EncoderConfig {
	cfg := testEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncodeLevel = zapcore.CapitalLevelEncoder
	cfg.EncodeDuration = zapcore.StringDurationEncoder
	return cfg
}

func getWriteSyncer(file string) zapcore.WriteSyncer {
	_, err := os.Stat(file)
	if os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(file), 0o744)
	}

	f, _ := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)

	return zapcore.AddSync(f)
}

// TestCoreOption test zapcore config option
func TestCoreOption(t *testing.T) {
	buf := new(bytes.Buffer)

	dynamicLevel := zap.NewAtomicLevel()

	dynamicLevel.SetLevel(zap.InfoLevel)

	logger := NewLogger(
		LoggerConfig{
			CoreConfigs: []CoreConfig{
				{
					Encoder:      zapcore.NewConsoleEncoder(humanEncoderConfig()),
					WriteSyncer:  zapcore.AddSync(os.Stdout),
					LevelEncoder: dynamicLevel,
				},
				{
					Encoder:      zapcore.NewJSONEncoder(humanEncoderConfig()),
					WriteSyncer:  getWriteSyncer("./all/log.log"),
					LevelEncoder: zap.NewAtomicLevelAt(zapcore.DebugLevel),
				},
				{
					Encoder:     zapcore.NewJSONEncoder(humanEncoderConfig()),
					WriteSyncer: getWriteSyncer("./debug/log.log"),
					LevelEncoder: zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
						return lev == zap.DebugLevel
					}),
				},
				{
					Encoder:     zapcore.NewJSONEncoder(humanEncoderConfig()),
					WriteSyncer: getWriteSyncer("./info/log.log"),
					LevelEncoder: zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
						return lev == zap.InfoLevel
					}),
				},
				{
					Encoder:     zapcore.NewJSONEncoder(humanEncoderConfig()),
					WriteSyncer: getWriteSyncer("./warn/log.log"),
					LevelEncoder: zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
						return lev == zap.WarnLevel
					}),
				},
				{
					Encoder:     zapcore.NewJSONEncoder(humanEncoderConfig()),
					WriteSyncer: getWriteSyncer("./error/log.log"),
					LevelEncoder: zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
						return lev >= zap.ErrorLevel
					}),
				},
			},
		})
	defer logger.Sync()

	logger.SetOutput(buf)

	logger.Debug("this is a debug log")
	// test log level
	assert.False(t, strings.Contains(buf.String(), "this is a debug log"))

	logger.Error("this is a warn log")
	// test log level
	assert.True(t, strings.Contains(buf.String(), "this is a warn log"))
	// test console encoder result
	assert.True(t, strings.Contains(buf.String(), "\tERROR\t"))

	logger.SetLevel(log.LevelDebug)
	logger.Debug("this is a debug log")
	assert.True(t, strings.Contains(buf.String(), "this is a debug log"))
}

func TestCoreConfigs(t *testing.T) {
	buf := new(bytes.Buffer)

	logger := NewLogger(LoggerConfig{
		CoreConfigs: []CoreConfig{
			{
				Encoder:      zapcore.NewConsoleEncoder(humanEncoderConfig()),
				LevelEncoder: zap.NewAtomicLevelAt(zap.WarnLevel),
				WriteSyncer:  zapcore.AddSync(buf),
			},
		},
	})
	defer logger.Sync()
	// output to buffer
	logger.SetOutput(buf)

	logger.Infof("this is a info log %s", "msg")
	assert.False(t, strings.Contains(buf.String(), "this is a info log"))
	logger.Warnf("this is a warn log %s", "msg")
	assert.True(t, strings.Contains(buf.String(), "this is a warn log"))
}

// TestCoreOptions test zapcore config option
func TestZapOptions(t *testing.T) {
	buf := new(bytes.Buffer)

	logger := NewLogger(
		LoggerConfig{
			ZapOptions: []zap.Option{
				zap.AddCaller(),
			},
		},
	)
	defer logger.Sync()

	logger.SetOutput(buf)

	logger.Debug("this is a debug log")
	assert.False(t, strings.Contains(buf.String(), "this is a debug log"))

	logger.Error("this is a warn log")
	// test caller in log result
	assert.True(t, strings.Contains(buf.String(), "caller"))
}

func TestWithContextCaller(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := NewLogger(LoggerConfig{
		ZapOptions: []zap.Option{
			zap.AddCaller(),
			zap.AddCallerSkip(3),
		},
	})
	logger.SetOutput(buf)

	logger.WithContext(context.Background()).Info("Hello, World!")
	var logStructMap map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &logStructMap)
	assert.Nil(t, err)
	value := logStructMap["caller"]
	assert.Equal(t, value, "fiberzap/logger_test.go:180")
}

// TestWithExtraKeys test WithExtraKeys option
func TestWithExtraKeys(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := NewLogger(LoggerConfig{
		ExtraKeys: []string{"requestId"},
	})
	logger.SetOutput(buf)

	ctx := context.WithValue(context.Background(), "requestId", "123")
	logger.WithContext(ctx).Infof("%s logger", "extra")

	var logStructMap map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &logStructMap)
	assert.Nil(t, err)

	value, ok := logStructMap["requestId"]

	assert.True(t, ok)
	assert.Equal(t, value, "123")
}

func BenchmarkNormal(b *testing.B) {
	buf := new(bytes.Buffer)
	log := NewLogger()
	log.SetOutput(buf)
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		log.WithContext(ctx).Info("normal log")
	}
}

func BenchmarkWithExtraKeys(b *testing.B) {
	buf := new(bytes.Buffer)
	logger := NewLogger(LoggerConfig{
		ExtraKeys: []string{"requestId"},
	})
	logger.SetOutput(buf)
	ctx := context.WithValue(context.Background(), "requestId", "123")
	for i := 0; i < b.N; i++ {
		logger.WithContext(ctx).Info("normal logger")
	}
}

func TestCustomField(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := NewLogger()
	log.SetLogger(logger)
	log.SetOutput(buf)
	log.Infow("", "test", "custom")
	var logStructMap map[string]interface{}

	err := json.Unmarshal(buf.Bytes(), &logStructMap)

	assert.Nil(t, err)

	value, ok := logStructMap["test"]

	assert.True(t, ok)
	assert.Equal(t, value, "custom")
}
