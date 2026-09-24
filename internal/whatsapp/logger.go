package whatsapp

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	LevelDebug = iota
	LevelInfo
	LevelWarn
	LevelError
)

func parseLogLevel(lvl string) int {
	switch strings.ToUpper(strings.TrimSpace(lvl)) {
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelWarn
	}
}

// FileLogger implements waLog.Logger, writing formatted log lines to a dedicated file
// instead of contaminating stdout/stderr and breaking the TUI.
type FileLogger struct {
	file     *os.File
	module   string
	minLevel int
	mu       *sync.Mutex
}

// NewFileLogger creates a file-backed logger or returns waLog.Noop if path is empty/none.
func NewFileLogger(filePath string, module string, minLevel string) (waLog.Logger, error) {
	cleanPath := strings.TrimSpace(filePath)
	if cleanPath == "" || cleanPath == "none" || cleanPath == "off" {
		return waLog.Noop, nil
	}

	f, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %q: %w", cleanPath, err)
	}

	return &FileLogger{
		file:     f,
		module:   module,
		minLevel: parseLogLevel(minLevel),
		mu:       &sync.Mutex{},
	}, nil
}

func (l *FileLogger) log(levelStr string, level int, msg string, args ...any) {
	if level < l.minLevel || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("15:04:05.000")
	formatted := fmt.Sprintf(msg, args...)
	_, _ = fmt.Fprintf(l.file, "%s [%s %s] %s\n", ts, l.module, levelStr, formatted)
}

func (l *FileLogger) Debugf(msg string, args ...any) { l.log("DEBUG", LevelDebug, msg, args...) }
func (l *FileLogger) Infof(msg string, args ...any)  { l.log("INFO", LevelInfo, msg, args...) }
func (l *FileLogger) Warnf(msg string, args ...any)  { l.log("WARN", LevelWarn, msg, args...) }
func (l *FileLogger) Errorf(msg string, args ...any) { l.log("ERROR", LevelError, msg, args...) }

func (l *FileLogger) Sub(module string) waLog.Logger {
	subModule := l.module
	if subModule != "" {
		subModule += "/" + module
	} else {
		subModule = module
	}
	return &FileLogger{
		file:     l.file,
		module:   subModule,
		minLevel: l.minLevel,
		mu:       l.mu,
	}
}
