package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
)

// systemHandler is an slog.Handler that emits records through a kardianos
// service.Logger. On Windows the underlying sink is the Windows Event Log
// under the service's name; on Unix it is syslog.
//
// Records are formatted as a single-line message of the shape:
//
//	<message> [k1=v1 k2=v2 ...]
//
// because the Windows Event Log presents each entry as opaque text - there is
// no native structured-data column. We also include a level prefix only for
// debug records (Info/Warn/Error map directly to event categories).
type systemHandler struct {
	logger service.Logger
	level  slog.Level
	attrs  []slog.Attr
	groups []string
}

func newSystemHandler(logger service.Logger, level slog.Level) *systemHandler {
	return &systemHandler{logger: logger, level: level}
}

func (h *systemHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

func (h *systemHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	if r.Level == slog.LevelDebug {
		sb.WriteString("[debug] ")
	}
	sb.WriteString(r.Message)

	// Pre-bound attrs first, then per-record attrs, both group-prefixed if
	// necessary.
	for _, a := range h.attrs {
		writeAttr(&sb, h.groups, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&sb, h.groups, a)
		return true
	})

	msg := sb.String()
	switch {
	case r.Level >= slog.LevelError:
		return h.logger.Error(msg)
	case r.Level >= slog.LevelWarn:
		return h.logger.Warning(msg)
	default:
		// Debug records map to Info because the Windows Event Log has no
		// dedicated debug level.
		return h.logger.Info(msg)
	}
}

func (h *systemHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

func (h *systemHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

func writeAttr(sb *strings.Builder, groups []string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		grp := append(append([]string{}, groups...), a.Key)
		for _, ga := range v.Group() {
			writeAttr(sb, grp, ga)
		}
		return
	}
	sb.WriteByte(' ')
	for _, g := range groups {
		sb.WriteString(g)
		sb.WriteByte('.')
	}
	sb.WriteString(a.Key)
	sb.WriteByte('=')
	sb.WriteString(formatValue(v))
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if needsQuote(s) {
			return strconv.Quote(s)
		}
		return s
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", v.Any())
	}
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' {
			return true
		}
	}
	return false
}

// dummyServiceLogger is exposed for tests that need a Logger without
// constructing a real service.
type dummyServiceLogger struct {
	mu      sync.Mutex
	records []string
}

func (d *dummyServiceLogger) Error(v ...any) error          { return d.write("ERROR", v) }
func (d *dummyServiceLogger) Warning(v ...any) error        { return d.write("WARN", v) }
func (d *dummyServiceLogger) Info(v ...any) error           { return d.write("INFO", v) }
func (d *dummyServiceLogger) Errorf(f string, a ...any) error {
	return d.writeString("ERROR", fmt.Sprintf(f, a...))
}

func (d *dummyServiceLogger) Warningf(f string, a ...any) error {
	return d.writeString("WARN", fmt.Sprintf(f, a...))
}

func (d *dummyServiceLogger) Infof(f string, a ...any) error {
	return d.writeString("INFO", fmt.Sprintf(f, a...))
}

func (d *dummyServiceLogger) write(level string, v []any) error {
	return d.writeString(level, fmt.Sprint(v...))
}

func (d *dummyServiceLogger) writeString(level, msg string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, level+" "+msg)
	return nil
}
