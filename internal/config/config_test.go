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
	path := filepath.Join(dir, DefaultFileName)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const minimalValid = `{
  "aws": { "region": "us-east-1" },
  "inbound": {
    "sqsQueueUrl": "https://sqs.us-east-1.amazonaws.com/123456789012/ses-smtp-proxy-inbound",
    "exchange": { "host": "exchange.local", "port": 25 }
  },
  "outbound": {
    "listen": "0.0.0.0:2525",
    "allowedCidrs": ["10.0.0.0/16", "127.0.0.1/32"]
  },
  "logging": { "level": "info" }
}`

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
		json string
		want string
	}{
		{
			name: "missing region",
			json: strings.Replace(minimalValid, `"region": "us-east-1"`, `"region": ""`, 1),
			want: "aws.region is required",
		},
		{
			name: "bad CIDR",
			json: strings.Replace(minimalValid, `"10.0.0.0/16"`, `"not-a-cidr"`, 1),
			want: "not a valid CIDR",
		},
		{
			name: "missing CIDRs",
			json: strings.Replace(minimalValid, `["10.0.0.0/16", "127.0.0.1/32"]`, `[]`, 1),
			want: "allowedCidrs must list at least one CIDR",
		},
		{
			name: "bad listen",
			json: strings.Replace(minimalValid, `"0.0.0.0:2525"`, `"not-a-host-port"`, 1),
			want: "outbound.listen",
		},
		{
			name: "partial creds",
			json: strings.Replace(minimalValid,
				`"aws": { "region": "us-east-1" }`,
				`"aws": { "region": "us-east-1", "accessKeyId": "AKIA" }`, 1),
			want: "must both be set or both empty",
		},
		{
			name: "bad log level",
			json: strings.Replace(minimalValid, `"level": "info"`, `"level": "verbose"`, 1),
			want: "logging.level",
		},
		{
			name: "poll wait too high",
			json: strings.Replace(minimalValid,
				`"sqsQueueUrl"`,
				`"pollWaitSeconds": 30, "sqsQueueUrl"`, 1),
			want: "inbound.pollWaitSeconds",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.json)
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
	// Inject a top-level unknown field.
	bad := strings.Replace(minimalValid, `"aws":`, `"bogusKey": 1, "aws":`, 1)
	path := writeTempConfig(t, bad)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "bogusKey") {
		t.Errorf("expected error to mention unknown field, got: %v", err)
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	path := writeTempConfig(t, minimalValid+"\n{\"second\": true}\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for trailing data")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("expected trailing-data error, got: %v", err)
	}
}

func TestResolveExplicitMissing(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

func TestResolveExeDirFallback(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable failed: %v", err)
	}
	exeDirConfig := filepath.Join(filepath.Dir(exe), DefaultFileName)
	if _, err := os.Stat(exeDirConfig); err == nil {
		t.Skipf("real config already exists at %s; cannot test the negative path", exeDirConfig)
	}

	_, err = Resolve("")
	if err == nil {
		t.Fatal("expected error when no config exists next to the test binary")
	}
	if !strings.Contains(err.Error(), exeDirConfig) {
		t.Errorf("error should mention the exe-dir path %q: %v", exeDirConfig, err)
	}
}
