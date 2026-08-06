// Package outbound implements the local SMTP listener that the on-premise
// Exchange server relays outbound mail to. Each accepted DATA payload is sent
// to Amazon SES via the SendEmail API using the SMTP envelope as the SES
// source/destination.
package outbound

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/awsclient"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// Server hosts the SMTP listener.
type Server struct {
	cfg     config.OutboundConfig
	clients *awsclient.Clients
	log     *slog.Logger

	smtpServer *smtp.Server
}

// New constructs an unstarted Server.
func New(cfg config.OutboundConfig, clients *awsclient.Clients, log *slog.Logger) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		clients: clients,
		log:     log.With("component", "outbound"),
	}

	be := &backend{
		allowed:        cfg.AllowedNets,
		nullSenderFrom: cfg.NullSenderFrom,
		ses:            clients.SES,
		log:            s.log,
	}

	srv := smtp.NewServer(be)
	srv.Addr = cfg.Listen
	srv.Domain = "ses-smtp-proxy"
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.MaxRecipients = 100
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	srv.AllowInsecureAuth = true
	srv.ErrorLog = slogErrorLogger{log: s.log}

	if cfg.TLS.TLSEnabled() {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load STARTTLS keypair: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	s.smtpServer = srv
	return s, nil
}

// Run serves SMTP until ctx is cancelled or a fatal error occurs.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting outbound SMTP server",
		"listen", s.cfg.Listen,
		"allowedCidrs", s.cfg.AllowedCIDRs,
		"tls", s.cfg.TLS.TLSEnabled(),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.smtpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.smtpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			s.log.Warn("smtp server shutdown error", "err", err)
		}
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, smtp.ErrServerClosed) || err == nil {
			return nil
		}
		return fmt.Errorf("smtp serve: %w", err)
	}
}

// backend implements smtp.Backend.
type backend struct {
	allowed        []*net.IPNet
	nullSenderFrom string
	ses            sesSender
	log            *slog.Logger
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	raddr := c.Conn().RemoteAddr()
	host, _, err := net.SplitHostPort(raddr.String())
	if err != nil {
		host = raddr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil || !ipAllowed(ip, b.allowed) {
		b.log.Warn("rejecting connection from disallowed source", "remoteAddr", raddr.String())
		return nil, &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message:      "Source IP not permitted to relay",
		}
	}
	return &session{
		log:            b.log.With("remoteAddr", raddr.String()),
		ses:            b.ses,
		nullSenderFrom: b.nullSenderFrom,
		recipients:     make([]string, 0, 4),
	}, nil
}

func ipAllowed(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// session implements smtp.Session and is recreated per SMTP connection.
type session struct {
	log            *slog.Logger
	ses            sesSender
	nullSenderFrom string
	from           string
	recipients     []string
}

func (s *session) Reset() {
	s.from = ""
	s.recipients = s.recipients[:0]
}

func (s *session) Logout() error { return nil }

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		s.log.Error("read DATA failed", "err", err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 0, 0},
			Message:      "Failed to read message body",
		}
	}

	log := s.log.With(
		"from", s.from,
		"to", s.recipients,
		"bytes", buf.Len(),
	)

	effectiveFrom := s.from
	if effectiveFrom == "" {
		effectiveFrom = s.nullSenderFrom
	}
	if effectiveFrom == "" {
		log.Warn("rejecting message with empty MAIL FROM")
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 5, 4},
			Message:      "MAIL FROM is required",
		}
	}
	if len(s.recipients) == 0 {
		log.Warn("rejecting message with no recipients")
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 5, 4},
			Message:      "At least one recipient is required",
		}
	}

	res := sendViaSES(context.Background(), s.ses, effectiveFrom, s.recipients, buf.Bytes())
	if res.err != nil {
		if res.permanent {
			log.Error("SES permanent failure", "err", res.err)
			return &smtp.SMTPError{
				Code:         554,
				EnhancedCode: smtp.EnhancedCode{5, 6, 0},
				Message:      describeSESError(res.err),
			}
		}
		log.Warn("SES transient failure", "err", res.err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 4, 1},
			Message:      describeSESError(res.err),
		}
	}

	log.Info("relayed email to SES", "sesMessageId", res.messageID)
	return nil
}

// slogErrorLogger adapts a slog.Logger to the go-smtp Logger interface used
// internally by the server for connection-handling errors.
type slogErrorLogger struct{ log *slog.Logger }

func (l slogErrorLogger) Printf(format string, v ...any) {
	l.log.Warn(fmt.Sprintf(format, v...))
}

func (l slogErrorLogger) Println(v ...any) {
	l.log.Warn(fmt.Sprintln(v...))
}
