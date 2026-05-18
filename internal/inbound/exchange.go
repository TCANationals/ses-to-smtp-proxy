package inbound

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"time"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// relayError wraps an SMTP error and reports whether the failure is
// permanent (5xx, message will never be accepted) or transient (4xx, network,
// etc. - retry later).
type relayError struct {
	err       error
	permanent bool
}

func (e *relayError) Error() string {
	if e.permanent {
		return "permanent SMTP error: " + e.err.Error()
	}
	return "transient SMTP error: " + e.err.Error()
}

func (e *relayError) Unwrap() error { return e.err }

// IsPermanent reports whether err is a permanent SMTP failure.
func IsPermanent(err error) bool {
	for err != nil {
		if r, ok := err.(*relayError); ok {
			return r.permanent
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// relayToExchange opens an SMTP session to Exchange and transmits a single
// message. It does not retry; the caller decides based on IsPermanent.
func relayToExchange(cfg config.ExchangeConfig, from string, to []string, raw []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return &relayError{err: fmt.Errorf("dial %s: %w", addr, err), permanent: false}
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return &relayError{err: fmt.Errorf("smtp handshake: %w", err), permanent: false}
	}
	defer func() { _ = client.Quit() }()

	helo := cfg.HeloDomain
	if helo == "" {
		helo = "ses-smtp-proxy.local"
	}
	if err := client.Hello(helo); err != nil {
		return wrapSMTPError(err, "EHLO")
	}

	if cfg.StartTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return &relayError{
				err:       fmt.Errorf("server does not advertise STARTTLS but starttls=true"),
				permanent: true,
			}
		}
		tlsCfg := &tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in via config
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return wrapSMTPError(err, "STARTTLS")
		}
	}

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return wrapSMTPError(err, "AUTH")
		}
	}

	if err := client.Mail(from); err != nil {
		return wrapSMTPError(err, "MAIL FROM")
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return wrapSMTPError(err, "RCPT TO "+rcpt)
		}
	}

	w, err := client.Data()
	if err != nil {
		return wrapSMTPError(err, "DATA")
	}
	if _, err := io.Copy(w, bytes.NewReader(raw)); err != nil {
		_ = w.Close()
		return &relayError{err: fmt.Errorf("write DATA: %w", err), permanent: false}
	}
	if err := w.Close(); err != nil {
		return wrapSMTPError(err, "DATA close")
	}
	return nil
}

// wrapSMTPError classifies an error from net/smtp as permanent or transient
// based on the leading status digit when present.
func wrapSMTPError(err error, op string) error {
	wrapped := fmt.Errorf("%s: %w", op, err)
	if s := err.Error(); len(s) >= 3 && s[0] == '5' {
		return &relayError{err: wrapped, permanent: true}
	}
	return &relayError{err: wrapped, permanent: false}
}
