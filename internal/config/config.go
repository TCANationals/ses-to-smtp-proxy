// Package config loads and validates the ses-smtp-proxy JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultFileName is the basename of the configuration file the service
// looks for next to the running executable.
const DefaultFileName = "config.json"

// Config is the top-level configuration object.
type Config struct {
	AWS      AWSConfig      `json:"aws"`
	Inbound  InboundConfig  `json:"inbound"`
	Outbound OutboundConfig `json:"outbound"`
	Logging  LoggingConfig  `json:"logging"`

	// Path is the absolute path the configuration was loaded from. Useful for
	// log messages.
	Path string `json:"-"`
}

// AWSConfig holds shared AWS SDK settings. All fields are optional; if no
// static credentials are supplied the default credential chain is used.
type AWSConfig struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Profile         string `json:"profile"`
}

// InboundConfig configures the SQS->SMTP relay direction.
type InboundConfig struct {
	SQSQueueURL              string         `json:"sqsQueueUrl"`
	S3Bucket                 string         `json:"s3Bucket"`
	PollWaitSeconds          int            `json:"pollWaitSeconds"`
	MaxConcurrent            int            `json:"maxConcurrent"`
	VisibilityTimeoutSeconds int            `json:"visibilityTimeoutSeconds"`
	Exchange                 ExchangeConfig `json:"exchange"`
}

// ExchangeConfig describes the on-premise Exchange SMTP target.
type ExchangeConfig struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	StartTLS           bool   `json:"starttls"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	HeloDomain         string `json:"heloDomain"`
	Username           string `json:"username"`
	Password           string `json:"password"`
}

// OutboundConfig configures the local SMTP listener that relays mail out
// through SES.
type OutboundConfig struct {
	Listen          string    `json:"listen"`
	AllowedCIDRs    []string  `json:"allowedCidrs"`
	MaxMessageBytes int64     `json:"maxMessageBytes"`
	TLS             TLSConfig `json:"tls"`

	// AllowedNets is the parsed form of AllowedCIDRs, populated by Validate.
	AllowedNets []*net.IPNet `json:"-"`
}

// TLSConfig configures STARTTLS for the local SMTP listener. Both fields must
// be set to enable TLS; leaving them empty disables STARTTLS.
type TLSConfig struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

// LoggingConfig configures slog output.
type LoggingConfig struct {
	Level      string `json:"level"`
	File       string `json:"file"`
	MaxSizeMB  int    `json:"maxSizeMB"`
	MaxBackups int    `json:"maxBackups"`
	MaxAgeDays int    `json:"maxAgeDays"`
}

// Load reads, parses, and validates the configuration. If explicitPath is
// empty, Resolve is used to find a configuration file.
func Load(explicitPath string) (*Config, error) {
	path, err := Resolve(explicitPath)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := defaultConfig()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse config %s: trailing data after the top-level object", path)
	}
	cfg.Path = path

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}
	return cfg, nil
}

// Resolve returns the absolute path of the configuration file using the
// documented search order:
//
//  1. explicitPath if non-empty
//  2. config.json next to the running executable
func Resolve(explicitPath string) (string, error) {
	if explicitPath != "" {
		abs, err := filepath.Abs(explicitPath)
		if err != nil {
			return "", fmt.Errorf("resolve --config: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("config file %s: %w", abs, err)
		}
		return abs, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), DefaultFileName)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("config file %s: %w (pass --config to override)", candidate, err)
	}
	return candidate, nil
}

func defaultConfig() *Config {
	return &Config{
		Inbound: InboundConfig{
			PollWaitSeconds:          20,
			MaxConcurrent:            4,
			VisibilityTimeoutSeconds: 300,
			Exchange: ExchangeConfig{
				Host:       "127.0.0.1",
				Port:       25,
				StartTLS:   true,
				HeloDomain: "ses-smtp-proxy.local",
			},
		},
		Outbound: OutboundConfig{
			Listen:          "127.0.0.1:2525",
			MaxMessageBytes: 41943040,
		},
		Logging: LoggingConfig{
			Level:      "info",
			MaxSizeMB:  50,
			MaxBackups: 5,
			MaxAgeDays: 30,
		},
	}
}

// Validate checks the configuration and populates derived fields.
func (c *Config) Validate() error {
	var errs []string

	if c.AWS.Region == "" {
		errs = append(errs, "aws.region is required")
	}
	if (c.AWS.AccessKeyID == "") != (c.AWS.SecretAccessKey == "") {
		errs = append(errs, "aws.accessKeyId and aws.secretAccessKey must both be set or both empty")
	}

	if c.Inbound.SQSQueueURL == "" {
		errs = append(errs, "inbound.sqsQueueUrl is required")
	} else if u, err := url.Parse(c.Inbound.SQSQueueURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, "inbound.sqsQueueUrl is not a valid URL")
	}
	if c.Inbound.PollWaitSeconds < 0 || c.Inbound.PollWaitSeconds > 20 {
		errs = append(errs, "inbound.pollWaitSeconds must be between 0 and 20")
	}
	if c.Inbound.MaxConcurrent <= 0 {
		errs = append(errs, "inbound.maxConcurrent must be > 0")
	}
	if c.Inbound.VisibilityTimeoutSeconds <= 0 {
		errs = append(errs, "inbound.visibilityTimeoutSeconds must be > 0")
	}
	if c.Inbound.Exchange.Host == "" {
		errs = append(errs, "inbound.exchange.host is required")
	}
	if c.Inbound.Exchange.Port <= 0 || c.Inbound.Exchange.Port > 65535 {
		errs = append(errs, "inbound.exchange.port must be 1-65535")
	}
	if (c.Inbound.Exchange.Username == "") != (c.Inbound.Exchange.Password == "") {
		errs = append(errs, "inbound.exchange.username and password must both be set or both empty")
	}

	if c.Outbound.Listen == "" {
		errs = append(errs, "outbound.listen is required")
	} else if _, _, err := net.SplitHostPort(c.Outbound.Listen); err != nil {
		errs = append(errs, fmt.Sprintf("outbound.listen %q is not host:port: %v", c.Outbound.Listen, err))
	}
	if c.Outbound.MaxMessageBytes <= 0 {
		errs = append(errs, "outbound.maxMessageBytes must be > 0")
	}
	if len(c.Outbound.AllowedCIDRs) == 0 {
		errs = append(errs, "outbound.allowedCidrs must list at least one CIDR")
	} else {
		c.Outbound.AllowedNets = make([]*net.IPNet, 0, len(c.Outbound.AllowedCIDRs))
		for _, raw := range c.Outbound.AllowedCIDRs {
			_, ipNet, err := net.ParseCIDR(raw)
			if err != nil {
				errs = append(errs, fmt.Sprintf("outbound.allowedCidrs: %q is not a valid CIDR: %v", raw, err))
				continue
			}
			c.Outbound.AllowedNets = append(c.Outbound.AllowedNets, ipNet)
		}
	}
	if (c.Outbound.TLS.CertFile == "") != (c.Outbound.TLS.KeyFile == "") {
		errs = append(errs, "outbound.tls.certFile and keyFile must both be set or both empty")
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Sprintf("logging.level %q is not one of debug|info|warn|error", c.Logging.Level))
	}
	if c.Logging.File != "" {
		if c.Logging.MaxSizeMB <= 0 {
			errs = append(errs, "logging.maxSizeMB must be > 0 when logging.file is set")
		}
		if c.Logging.MaxBackups < 0 {
			errs = append(errs, "logging.maxBackups must be >= 0")
		}
		if c.Logging.MaxAgeDays < 0 {
			errs = append(errs, "logging.maxAgeDays must be >= 0")
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// TLSEnabled reports whether the outbound SMTP listener should enable STARTTLS.
func (t TLSConfig) TLSEnabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}
