// Package logging configures slog with either a rotated file handler or a
// Windows Event Log handler that forwards records through a kardianos
// service.Logger.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/kardianos/service"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// Mode selects which logging backend to use.
type Mode int

const (
	// ModeAuto picks ModeFile if cfg.Logging.File is set, otherwise
	// ModeSystem when sysLogger is non-nil, otherwise ModeStderr.
	ModeAuto Mode = iota
	// ModeStderr writes structured logs to os.Stderr.
	ModeStderr
	// ModeFile writes structured logs to a rotated file at cfg.Logging.File.
	ModeFile
	// ModeSystem writes through a kardianos service.Logger (Windows Event Log
	// on Windows; syslog on Unix).
	ModeSystem
)

// Setup configures slog's default logger from cfg and the given mode.
// sysLogger may be nil when running interactively. When non-nil it is used by
// ModeSystem (and by ModeAuto when no file is configured).
//
// The returned closer should be invoked at process shutdown to flush any
// file-based sink.
func Setup(cfg config.LoggingConfig, mode Mode, sysLogger service.Logger) (io.Closer, error) {
	level := parseLevel(cfg.Level)

	resolved := mode
	if resolved == ModeAuto {
		switch {
		case cfg.File != "":
			resolved = ModeFile
		case sysLogger != nil:
			resolved = ModeSystem
		default:
			resolved = ModeStderr
		}
	}

	var (
		handler slog.Handler
		closer  io.Closer = nopCloser{}
	)

	switch resolved {
	case ModeStderr:
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	case ModeFile:
		if cfg.File == "" {
			return nil, fmt.Errorf("logging mode=file but logging.file is empty")
		}
		lj := &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   true,
		}
		handler = slog.NewJSONHandler(lj, &slog.HandlerOptions{Level: level})
		closer = lj
	case ModeSystem:
		if sysLogger == nil {
			return nil, fmt.Errorf("logging mode=system but no system logger available")
		}
		handler = newSystemHandler(sysLogger, level)
	default:
		return nil, fmt.Errorf("unknown logging mode %d", resolved)
	}

	slog.SetDefault(slog.New(handler))
	return closer, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
