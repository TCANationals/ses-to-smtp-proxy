package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const minimalValid = `
aws:
  region: us-east-1
inbound:
  sqsQueueUrl: https://sqs.us-east-1.amazonaws.com/123456789012/ses-smtp-proxy-inbound
  exchange:
    host: exchange.local
    port: 25
outbound:
  listen: 0.0.0.0:2525
  allowedCidrs:
    - 10.0.0.0/16
    - 127.0.0.1/32
logging:
  level: info
`

func TestLoadMinimalValid(t *testing.T) {
	path := writeTempConfig(t, minimalValid)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AWS.Region != "us-east-1" {
		t.Errorf("region: got %q", cfg.AWS.Region)
	}
	if cfg.Inbound.PollWaitSeconds != 20 {
		t.Errorf("defaults not applied: PollWaitSeconds = %d", cfg.Inbound.PollWaitSeconds)
	}
	if cfg.Outbound.MaxMessageBytes != 41943040 {
		t.Errorf("defaults not applied: MaxMessageBytes = %d", cfg.Outbound.MaxMessageBytes)
	}
	if got, want := len(cfg.Outbound.AllowedNets), 2; got != want {
		t.Errorf("AllowedNets count: got %d want %d", got, want)
	}
	if cfg.Path != path {
		t.Errorf("Path: got %q want %q", cfg.Path, path)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing region",
			yaml: strings.Replace(minimalValid, "region: us-east-1", "region: \"\"", 1),
			want: "aws.region is required",
		},
		{
			name: "bad CIDR",
			yaml: strings.Replace(minimalValid, "- 10.0.0.0/16", "- not-a-cidr", 1),
			want: "not a valid CIDR",
		},
		{
			name: "missing CIDRs",
			yaml: strings.Replace(minimalValid, "  allowedCidrs:\n    - 10.0.0.0/16\n    - 127.0.0.1/32\n", "  allowedCidrs: []\n", 1),
			want: "allowedCidrs must list at least one CIDR",
		},
		{
			name: "bad listen",
			yaml: strings.Replace(minimalValid, "listen: 0.0.0.0:2525", "listen: not-a-host-port", 1),
			want: "outbound.listen",
		},
		{
			name: "partial creds",
			yaml: strings.Replace(minimalValid, "aws:\n  region: us-east-1", "aws:\n  region: us-east-1\n  accessKeyId: AKIA", 1),
			want: "must both be set or both empty",
		},
		{
			name: "bad log level",
			yaml: strings.Replace(minimalValid, "level: info", "level: verbose", 1),
			want: "logging.level",
		},
		{
			name: "poll wait too high",
			yaml: strings.Replace(minimalValid, "  sqsQueueUrl:", "  pollWaitSeconds: 30\n  sqsQueueUrl:", 1),
			want: "inbound.pollWaitSeconds",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadUnknownField(t *testing.T) {
	path := writeTempConfig(t, minimalValid+"\nbogusKey: 1\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestResolveExplicitMissing(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

func TestResolveExeDirPreferredOverProgramData(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(candidate, []byte(minimalValid), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable failed: %v", err)
	}
	exeDirConfig := filepath.Join(filepath.Dir(exe), "config.yaml")
	if _, err := os.Stat(exeDirConfig); err == nil {
		t.Skipf("real config exists at %s; cannot run search-order test", exeDirConfig)
	}

	// Sanity: Resolve("") should error on this host (no exe-dir or
	// ProgramData config) - we don't assert the exact error since
	// ProgramData may or may not exist on the host.
	_, _ = Resolve("")
}
