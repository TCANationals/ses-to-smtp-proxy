package logging

import (
	"log/slog"
	"strings"
	"testing"
)

func TestSystemHandlerRoutesLevels(t *testing.T) {
	sink := &dummyServiceLogger{}
	h := newSystemHandler(sink, slog.LevelDebug)
	logger := slog.New(h).With("service", "ses-smtp-proxy")

	logger.Debug("starting up")
	logger.Info("hello", "user", "alice")
	logger.Warn("disk getting full", "pct", 92)
	logger.Error("rejecting bad input", "addr", "10.0.0.5", "reason", "denied")

	if got, want := len(sink.records), 4; got != want {
		t.Fatalf("records=%d want %d (%v)", got, want, sink.records)
	}

	if !strings.HasPrefix(sink.records[0], "INFO ") {
		t.Errorf("debug should map to INFO: %q", sink.records[0])
	}
	if !strings.Contains(sink.records[0], "[debug]") {
		t.Errorf("debug should be prefixed: %q", sink.records[0])
	}
	if !strings.HasPrefix(sink.records[1], "INFO ") {
		t.Errorf("info: %q", sink.records[1])
	}
	if !strings.Contains(sink.records[1], "service=ses-smtp-proxy") {
		t.Errorf("info missing pre-bound attr: %q", sink.records[1])
	}
	if !strings.HasPrefix(sink.records[2], "WARN ") {
		t.Errorf("warn: %q", sink.records[2])
	}
	if !strings.Contains(sink.records[2], "pct=92") {
		t.Errorf("warn missing attr: %q", sink.records[2])
	}
	if !strings.HasPrefix(sink.records[3], "ERROR ") {
		t.Errorf("error: %q", sink.records[3])
	}
	if !strings.Contains(sink.records[3], "addr=10.0.0.5") {
		t.Errorf("error missing addr: %q", sink.records[3])
	}
}

func TestSystemHandlerRespectsLevel(t *testing.T) {
	sink := &dummyServiceLogger{}
	h := newSystemHandler(sink, slog.LevelWarn)
	logger := slog.New(h)

	logger.Debug("nope")
	logger.Info("also nope")
	logger.Warn("yes")
	logger.Error("yes")

	if got, want := len(sink.records), 2; got != want {
		t.Fatalf("records=%d want %d (%v)", got, want, sink.records)
	}
}

func TestSystemHandlerQuotesValuesWithSpaces(t *testing.T) {
	sink := &dummyServiceLogger{}
	h := newSystemHandler(sink, slog.LevelDebug)
	slog.New(h).Info("hi", "msg", "two words")

	if !strings.Contains(sink.records[0], `msg="two words"`) {
		t.Errorf("expected quoted value, got: %q", sink.records[0])
	}
}

func TestSystemHandlerHandlesGroups(t *testing.T) {
	sink := &dummyServiceLogger{}
	h := newSystemHandler(sink, slog.LevelDebug)
	slog.New(h).WithGroup("aws").Info("ok", "region", "us-east-1")

	if !strings.Contains(sink.records[0], "aws.region=us-east-1") {
		t.Errorf("missing group prefix: %q", sink.records[0])
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v want %v", in, got, want)
		}
	}
}
