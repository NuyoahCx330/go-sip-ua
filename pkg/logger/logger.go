// Package logger 提供高性能、无锁写入的日志系统，支持结构化字段和多种输出后端。
package logger

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogLevel 日志级别定义。
type LogLevel int32

const (
	LevelTrace LogLevel = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

// String 返回日志级别的可读字符串。
func (l LogLevel) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelOff:
		return "OFF"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel 将字符串解析为 LogLevel。
func ParseLogLevel(s string) LogLevel {
	switch strings.ToUpper(s) {
	case "TRACE":
		return LevelTrace
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARN":
		return LevelWarn
	case "ERROR":
		return LevelError
	case "OFF":
		return LevelOff
	default:
		return LevelInfo
	}
}

// OutputCallback 日志输出回调函数签名。
type OutputCallback func(level LogLevel, module string, timestamp time.Time, message string, fields map[string]interface{})

// Config 日志系统配置。
type Config struct {
	Level           LogLevel
	EnableCaller    bool
	EnableTimestamp bool
	TimestampFormat string
	Output          io.Writer
	Callback        OutputCallback
	BufferSize      int
}

// DefaultConfig 返回默认的日志配置。
func DefaultConfig() Config {
	return Config{
		Level:           LevelInfo,
		EnableCaller:    false,
		EnableTimestamp: true,
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
		Output:          os.Stdout,
		BufferSize:      4096,
	}
}

// Logger 日志接口，支持结构化字段和并发安全写入。
type Logger interface {
	Trace(module string, format string, args ...interface{})
	Debug(module string, format string, args ...interface{})
	Info(module string, format string, args ...interface{})
	Warn(module string, format string, args ...interface{})
	Error(module string, format string, args ...interface{})
	SetOutputCallback(cb OutputCallback) error
	SetLevel(level LogLevel) error
	GetLevel() LogLevel
	WithFields(fields map[string]interface{}) Logger
	Flush() error
}

// logEntry 表示一条待写入的日志记录。
type logEntry struct {
	level     LogLevel
	module    string
	timestamp time.Time
	message   string
	fields    map[string]interface{}
	caller    string
}

// defaultLogger 是 Logger 接口的默认实现，使用环形缓冲区和异步写入。
type defaultLogger struct {
	level    atomic.Int32
	config   Config
	mu       sync.RWMutex
	fields   map[string]interface{}
	entryCh  chan *logEntry
	doneCh   chan struct{}
	writer   io.Writer
	callback OutputCallback
}

// New 创建一个新的日志实例。
func New(cfg Config) Logger {
	l := &defaultLogger{
		config:   cfg,
		fields:   make(map[string]interface{}),
		entryCh:  make(chan *logEntry, cfg.BufferSize),
		doneCh:   make(chan struct{}),
		writer:   cfg.Output,
		callback: cfg.Callback,
	}
	l.level.Store(int32(cfg.Level))

	if l.writer == nil {
		l.writer = os.Stdout
	}

	go l.writeLoop()
	return l
}

// NewDefault 使用默认配置创建日志实例。
func NewDefault() Logger {
	return New(DefaultConfig())
}

func (l *defaultLogger) log(level LogLevel, module string, format string, args ...interface{}) {
	if LogLevel(l.level.Load()) > level {
		return
	}

	msg := fmt.Sprintf(format, args...)
	entry := &logEntry{
		level:     level,
		module:    module,
		timestamp: time.Now(),
		message:   msg,
	}

	if l.config.EnableCaller {
		pc, file, line, ok := runtime.Caller(2)
		if ok {
			fn := runtime.FuncForPC(pc)
			funcName := "unknown"
			if fn != nil {
				funcName = fn.Name()
			}
			entry.caller = fmt.Sprintf("%s:%d %s", file, line, funcName)
		}
	}

	l.mu.RLock()
	if len(l.fields) > 0 {
		entry.fields = make(map[string]interface{}, len(l.fields))
		for k, v := range l.fields {
			entry.fields[k] = v
		}
	}
	l.mu.RUnlock()

	select {
	case l.entryCh <- entry:
	default:
		// 缓冲区满，丢弃日志防止阻塞
	}
}

func (l *defaultLogger) Trace(module string, format string, args ...interface{}) {
	l.log(LevelTrace, module, format, args...)
}

func (l *defaultLogger) Debug(module string, format string, args ...interface{}) {
	l.log(LevelDebug, module, format, args...)
}

func (l *defaultLogger) Info(module string, format string, args ...interface{}) {
	l.log(LevelInfo, module, format, args...)
}

func (l *defaultLogger) Warn(module string, format string, args ...interface{}) {
	l.log(LevelWarn, module, format, args...)
}

func (l *defaultLogger) Error(module string, format string, args ...interface{}) {
	l.log(LevelError, module, format, args...)
}

func (l *defaultLogger) SetOutputCallback(cb OutputCallback) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.callback = cb
	return nil
}

func (l *defaultLogger) SetLevel(level LogLevel) error {
	l.level.Store(int32(level))
	return nil
}

func (l *defaultLogger) GetLevel() LogLevel {
	return LogLevel(l.level.Load())
}

func (l *defaultLogger) WithFields(fields map[string]interface{}) Logger {
	l.mu.RLock()
	newFields := make(map[string]interface{}, len(l.fields)+len(fields))
	for k, v := range l.fields {
		newFields[k] = v
	}
	for k, v := range fields {
		newFields[k] = v
	}
	l.mu.RUnlock()

	child := &defaultLogger{
		config:   l.config,
		fields:   newFields,
		entryCh:  l.entryCh,
		doneCh:   l.doneCh,
		writer:   l.writer,
		callback: l.callback,
	}
	child.level.Store(l.level.Load())
	return child
}

func (l *defaultLogger) Flush() error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for {
		select {
		case entry := <-l.entryCh:
			l.writeEntry(entry)
		default:
			return nil
		}
	}
}

func (l *defaultLogger) writeLoop() {
	for {
		select {
		case entry := <-l.entryCh:
			l.writeEntry(entry)
		case <-l.doneCh:
			// 排空剩余日志
			for {
				select {
				case entry := <-l.entryCh:
					l.writeEntry(entry)
				default:
					return
				}
			}
		}
	}
}

func (l *defaultLogger) writeEntry(entry *logEntry) {
	l.mu.RLock()
	cb := l.callback
	w := l.writer
	cfg := l.config
	l.mu.RUnlock()

	if cb != nil {
		cb(entry.level, entry.module, entry.timestamp, entry.message, entry.fields)
		return
	}

	var buf strings.Builder
	if cfg.EnableTimestamp {
		buf.WriteString(entry.timestamp.Format(cfg.TimestampFormat))
		buf.WriteByte(' ')
	}
	buf.WriteByte('[')
	buf.WriteString(entry.level.String())
	buf.WriteString("] ")
	buf.WriteByte('[')
	buf.WriteString(entry.module)
	buf.WriteString("] ")
	buf.WriteString(entry.message)

	if entry.caller != "" {
		buf.WriteString(" (")
		buf.WriteString(entry.caller)
		buf.WriteByte(')')
	}

	if len(entry.fields) > 0 {
		buf.WriteString(" {")
		first := true
		for k, v := range entry.fields {
			if !first {
				buf.WriteString(", ")
			}
			buf.WriteString(k)
			buf.WriteString("=")
			buf.WriteString(fmt.Sprintf("%v", v))
			first = false
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('\n')

	_, _ = io.WriteString(w, buf.String())
}

// NopLogger 返回一个不输出任何内容的空日志实例，用于测试或禁用日志场景。
func NopLogger() Logger {
	return &nopLogger{}
}

type nopLogger struct{}

func (n *nopLogger) Trace(string, string, ...interface{})     {}
func (n *nopLogger) Debug(string, string, ...interface{})     {}
func (n *nopLogger) Info(string, string, ...interface{})      {}
func (n *nopLogger) Warn(string, string, ...interface{})      {}
func (n *nopLogger) Error(string, string, ...interface{})     {}
func (n *nopLogger) SetOutputCallback(OutputCallback) error   { return nil }
func (n *nopLogger) SetLevel(LogLevel) error                  { return nil }
func (n *nopLogger) GetLevel() LogLevel                       { return LevelOff }
func (n *nopLogger) WithFields(map[string]interface{}) Logger { return n }
func (n *nopLogger) Flush() error                             { return nil }
