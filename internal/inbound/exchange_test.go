package inbound

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// fakeSMTPServer is a minimal SMTP responder that records the DATA payload
// and the envelope used. It speaks just enough of the protocol for net/smtp
// to send a message without STARTTLS or AUTH.
type fakeSMTPServer struct {
	t        *testing.T
	listener net.Listener
	addr     string

	mu               sync.Mutex
	from             string
	to               []string
	data             strings.Builder
	gotQuit          bool
	rejectRecipients map[string]string
}

func newFakeSMTPServer(t *testing.T, rejectRecipients map[string]string) *fakeSMTPServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSMTPServer{
		t:                t,
		listener:         l,
		addr:             l.Addr().String(),
		rejectRecipients: rejectRecipients,
	}
	go srv.accept()
	return srv
}

func (s *fakeSMTPServer) accept() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	write("220 fake.smtp.test ready")

	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				write("250 2.0.0 queued")
				continue
			}
			s.mu.Lock()
			s.data.WriteString(line)
			s.data.WriteString("\r\n")
			s.mu.Unlock()
			continue
		}

		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-fake.smtp.test")
			write("250 SIZE 41943040")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.from = stripAngle(strings.TrimSpace(line[len("MAIL FROM:"):]))
			s.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			rcpt := stripAngle(strings.TrimSpace(line[len("RCPT TO:"):]))
			if response, reject := s.rejectRecipients[rcpt]; reject {
				write(response)
				continue
			}
			s.mu.Lock()
			s.to = append(s.to, rcpt)
			s.mu.Unlock()
			write("250 OK")
		case upper == "DATA":
			write("354 send")
			inData = true
		case upper == "QUIT":
			s.mu.Lock()
			s.gotQuit = true
			s.mu.Unlock()
			write("221 bye")
			return
		default:
			write("250 OK")
		}
	}
}

func TestRelayToExchangeContinuesAfterPermanentRecipientRejection(t *testing.T) {
	srv := newFakeSMTPServer(t, map[string]string{
		"missing@example.com": "550 5.1.1 mailbox unavailable",
	})
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ExchangeConfig{Host: host, Port: atoi(t, port), HeloDomain: "test.local"}
	raw := []byte("Subject: partial\r\n\r\nhello\r\n")
	err = relayToExchange(cfg, "sender@external.example",
		[]string{"missing@example.com", "alice@example.com"}, raw)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("relay error = %v, want permanent partial-recipient error", err)
	}
	failures := rejectedRecipients(err)
	if len(failures) != 1 || failures[0].Recipient != "missing@example.com" {
		t.Fatalf("rejected recipients = %#v", failures)
	}
	if got := acceptedRecipientCount(err); got != 1 {
		t.Fatalf("accepted recipient count = %d, want 1", got)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.to) != 1 || srv.to[0] != "alice@example.com" {
		t.Fatalf("accepted recipients = %v", srv.to)
	}
	if !strings.Contains(srv.data.String(), "hello") {
		t.Fatalf("accepted recipient did not receive DATA: %q", srv.data.String())
	}
}

func (s *fakeSMTPServer) Close() {
	_ = s.listener.Close()
}

func stripAngle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return s
}

func TestRelayToExchange(t *testing.T) {
	srv := newFakeSMTPServer(t, nil)
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ExchangeConfig{
		Host:       host,
		Port:       atoi(t, port),
		StartTLS:   false,
		HeloDomain: "test.local",
	}

	raw := []byte("Subject: hello\r\nFrom: bob@external.example\r\nTo: alice@example.com\r\n\r\nhi there\r\n")
	if err := relayToExchange(cfg, "bob@external.example", []string{"alice@example.com"}, raw); err != nil {
		t.Fatalf("relay: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.from != "bob@external.example" {
		t.Errorf("from: %q", srv.from)
	}
	if len(srv.to) != 1 || srv.to[0] != "alice@example.com" {
		t.Errorf("to: %v", srv.to)
	}
	if !strings.Contains(srv.data.String(), "hi there") {
		t.Errorf("data missing body: %q", srv.data.String())
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("bad port %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
