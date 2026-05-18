package outbound

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	smithy "github.com/aws/smithy-go"
	gosmtp "github.com/emersion/go-smtp"
)

type fakeSES struct {
	mu     sync.Mutex
	calls  int32
	last   *sesv2.SendEmailInput
	result func(in *sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error)
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.last = in
	f.mu.Unlock()
	if f.result != nil {
		return f.result(in)
	}
	return &sesv2.SendEmailOutput{MessageId: aws.String("fake-message-id")}, nil
}

func newBackend(t *testing.T, allowed []string, ses sesSender) *backend {
	t.Helper()
	nets := make([]*net.IPNet, 0, len(allowed))
	for _, c := range allowed {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("bad CIDR %q: %v", c, err)
		}
		nets = append(nets, n)
	}
	return &backend{
		allowed: nets,
		ses:     ses,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// startTestServer spins up a real go-smtp server on 127.0.0.1:0 wired to the
// given backend. It returns the chosen address and a shutdown func.
func startTestServer(t *testing.T, be *backend) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := gosmtp.NewServer(be)
	srv.Domain = "test"
	srv.MaxMessageBytes = 1 << 20
	srv.ReadTimeout = 5 * time.Second
	srv.WriteTimeout = 5 * time.Second
	srv.AllowInsecureAuth = true
	srv.ErrorLog = slogErrorLogger{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	done := make(chan struct{})
	go func() {
		_ = srv.Serve(l)
		close(done)
	}()
	return l.Addr().String(), func() {
		_ = srv.Close()
		<-done
	}
}

func TestSession_RelaysToSES(t *testing.T) {
	ses := &fakeSES{}
	be := newBackend(t, []string{"127.0.0.0/8"}, ses)
	addr, stop := startTestServer(t, be)
	defer stop()

	if err := smtp.SendMail(addr, nil, "bob@external.example",
		[]string{"alice@example.com", "carol@example.com"},
		[]byte("Subject: hi\r\nFrom: bob@external.example\r\nTo: alice@example.com\r\n\r\nhi\r\n"),
	); err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	if got := atomic.LoadInt32(&ses.calls); got != 1 {
		t.Fatalf("SES called %d times, want 1", got)
	}
	ses.mu.Lock()
	defer ses.mu.Unlock()
	if aws.ToString(ses.last.FromEmailAddress) != "bob@external.example" {
		t.Errorf("FromEmailAddress = %q", aws.ToString(ses.last.FromEmailAddress))
	}
	if got, want := ses.last.Destination.ToAddresses, []string{"alice@example.com", "carol@example.com"}; !equalSlice(got, want) {
		t.Errorf("ToAddresses = %v want %v", got, want)
	}
	if !bytes.Contains(ses.last.Content.Raw.Data, []byte("Subject: hi")) {
		t.Errorf("raw content missing subject: %q", ses.last.Content.Raw.Data)
	}
}

func TestSession_RejectsDisallowedIP(t *testing.T) {
	ses := &fakeSES{}
	be := newBackend(t, []string{"10.0.0.0/8"}, ses)
	addr, stop := startTestServer(t, be)
	defer stop()

	err := smtp.SendMail(addr, nil, "bob@external.example",
		[]string{"alice@example.com"},
		[]byte("hi\r\n"),
	)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.Contains(err.Error(), "554") {
		t.Errorf("expected 554, got %v", err)
	}
	if atomic.LoadInt32(&ses.calls) != 0 {
		t.Errorf("SES should not have been called")
	}
}

type stubAPIError struct {
	code  string
	fault smithy.ErrorFault
}

func (e *stubAPIError) Error() string          { return e.code + ": stub" }
func (e *stubAPIError) ErrorCode() string      { return e.code }
func (e *stubAPIError) ErrorMessage() string   { return "stub" }
func (e *stubAPIError) ErrorFault() smithy.ErrorFault { return e.fault }

func TestSession_TransientSESErrorBecomes451(t *testing.T) {
	ses := &fakeSES{
		result: func(*sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error) {
			return nil, &stubAPIError{code: "ThrottlingException", fault: smithy.FaultClient}
		},
	}
	be := newBackend(t, []string{"127.0.0.0/8"}, ses)
	addr, stop := startTestServer(t, be)
	defer stop()

	err := smtp.SendMail(addr, nil, "bob@external.example",
		[]string{"alice@example.com"}, []byte("hi\r\n"))
	if err == nil {
		t.Fatal("expected transient error")
	}
	if !strings.Contains(err.Error(), "451") {
		t.Errorf("expected 451, got %v", err)
	}
}

func TestSession_PermanentSESErrorBecomes554(t *testing.T) {
	ses := &fakeSES{
		result: func(*sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error) {
			return nil, &stubAPIError{code: "MessageRejected", fault: smithy.FaultClient}
		},
	}
	be := newBackend(t, []string{"127.0.0.0/8"}, ses)
	addr, stop := startTestServer(t, be)
	defer stop()

	err := smtp.SendMail(addr, nil, "bob@external.example",
		[]string{"alice@example.com"}, []byte("hi\r\n"))
	if err == nil {
		t.Fatal("expected permanent error")
	}
	if !strings.Contains(err.Error(), "554") {
		t.Errorf("expected 554, got %v", err)
	}
}

func TestIsPermanentSESError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("network"), false},
		{&stubAPIError{code: "ThrottlingException", fault: smithy.FaultClient}, false},
		{&stubAPIError{code: "TooManyRequestsException", fault: smithy.FaultClient}, false},
		{&stubAPIError{code: "MessageRejected", fault: smithy.FaultClient}, true},
		{&stubAPIError{code: "AccountSuspendedException", fault: smithy.FaultClient}, true},
		{&stubAPIError{code: "InternalServerError", fault: smithy.FaultServer}, false},
		{&stubAPIError{code: "ValidationException", fault: smithy.FaultClient}, true},
	}
	for _, tc := range cases {
		if got := isPermanentSESError(tc.err); got != tc.want {
			t.Errorf("isPermanentSESError(%v) = %v want %v", tc.err, got, tc.want)
		}
	}
}

func TestDescribeSESError(t *testing.T) {
	long := strings.Repeat("a", 500)
	out := describeSESError(errors.New(long))
	if len(out) > 210 {
		t.Errorf("description not truncated: %d", len(out))
	}
	out = describeSESError(errors.New("bad\x00thing"))
	if strings.ContainsRune(out, 0) {
		t.Errorf("description contains control char: %q", out)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
