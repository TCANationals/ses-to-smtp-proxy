package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/mail"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

type fakeWorkerSQS struct {
	deleteCalls int
}

func (f *fakeWorkerSQS) ReceiveMessage(
	context.Context,
	*sqs.ReceiveMessageInput,
	...func(*sqs.Options),
) (*sqs.ReceiveMessageOutput, error) {
	return nil, errors.New("unexpected ReceiveMessage call")
}

func (f *fakeWorkerSQS) DeleteMessage(
	_ context.Context,
	_ *sqs.DeleteMessageInput,
	_ ...func(*sqs.Options),
) (*sqs.DeleteMessageOutput, error) {
	f.deleteCalls++
	return &sqs.DeleteMessageOutput{}, nil
}

type fakeWorkerS3 struct {
	raw      []byte
	getCalls int
}

func (f *fakeWorkerS3) GetObject(
	_ context.Context,
	_ *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	f.getCalls++
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.raw))}, nil
}

type fakeWorkerSES struct {
	inputs []*sesv2.SendEmailInput
	err    error
}

func (f *fakeWorkerSES) SendEmail(
	_ context.Context,
	in *sesv2.SendEmailInput,
	_ ...func(*sesv2.Options),
) (*sesv2.SendEmailOutput, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sesv2.SendEmailOutput{}, nil
}

func TestRecipientsMatchSuffix(t *testing.T) {
	tests := []struct {
		name       string
		recipients []string
		suffix     string
		want       bool
	}{
		{name: "disabled", recipients: []string{"alice@example.com"}, want: true},
		{name: "all match", recipients: []string{"alice.42@example.com", "BOB.42@EXAMPLE.COM"}, suffix: ".42@example.com", want: true},
		{name: "other environment", recipients: []string{"alice.42@example.com", "bob.7@example.com"}, suffix: ".42@example.com", want: false},
		{name: "invalid address with matching suffix", recipients: []string{"alice@evil.42@example.com"}, suffix: ".42@example.com", want: false},
		{name: "empty", suffix: ".42@example.com", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipientsMatchSuffix(tc.recipients, tc.suffix); got != tc.want {
				t.Fatalf("recipientsMatchSuffix() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleMessageRejectsRecipientOutsideSuffixBeforeS3Fetch(t *testing.T) {
	sqsClient := &fakeWorkerSQS{}
	s3Client := &fakeWorkerS3{}
	sesClient := &fakeWorkerSES{}
	w := newTestWorker(t, sqsClient, s3Client, sesClient, "127.0.0.1", 25)

	w.handleMessage(context.Background(), notificationMessage(t,
		"sender@external.example", []string{"alice.7@example.com"}))

	if s3Client.getCalls != 0 {
		t.Fatalf("S3 GetObject calls = %d, want 0", s3Client.getCalls)
	}
	if len(sesClient.inputs) != 0 {
		t.Fatalf("SES SendEmail calls = %d, want 0", len(sesClient.inputs))
	}
	if sqsClient.deleteCalls != 1 {
		t.Fatalf("SQS DeleteMessage calls = %d, want 1", sqsClient.deleteCalls)
	}
}

func TestHandleMessagePartialRecipientRejectionSendsDSNAndDeletes(t *testing.T) {
	smtpServer := newFakeSMTPServer(t, map[string]string{
		"missing.42@example.com": "550 5.1.1 mailbox unavailable",
	})
	defer smtpServer.Close()

	host, portString, found := strings.Cut(smtpServer.addr, ":")
	if !found || host == "" {
		t.Fatalf("split SMTP address %q", smtpServer.addr)
	}

	sqsClient := &fakeWorkerSQS{}
	s3Client := &fakeWorkerS3{raw: []byte(
		"From: sender@external.example\r\nSubject: original\r\n\r\nsecret body\r\n")}
	sesClient := &fakeWorkerSES{}
	w := newTestWorker(t, sqsClient, s3Client, sesClient, host, atoi(t, portString))

	w.handleMessage(context.Background(), notificationMessage(t,
		"sender@external.example",
		[]string{"missing.42@example.com", "alice.42@example.com"}))

	if sqsClient.deleteCalls != 1 {
		t.Fatalf("SQS DeleteMessage calls = %d, want 1", sqsClient.deleteCalls)
	}
	if len(sesClient.inputs) != 1 {
		t.Fatalf("SES SendEmail calls = %d, want 1", len(sesClient.inputs))
	}
	sent := sesClient.inputs[0]
	if got := aws.ToString(sent.FromEmailAddress); got != "postmaster.42@example.com" {
		t.Fatalf("SES FromEmailAddress = %q", got)
	}
	if sent.Destination == nil || len(sent.Destination.ToAddresses) != 1 ||
		sent.Destination.ToAddresses[0] != "sender@external.example" {
		t.Fatalf("SES Destination = %#v", sent.Destination)
	}
	dsn := string(sent.Content.Raw.Data)
	if !strings.Contains(dsn, "Final-Recipient: rfc822; missing.42@example.com") {
		t.Fatalf("DSN does not name rejected recipient:\n%s", dsn)
	}
	if strings.Contains(dsn, "Final-Recipient: rfc822; alice.42@example.com") {
		t.Fatalf("DSN incorrectly names accepted recipient:\n%s", dsn)
	}

	smtpServer.mu.Lock()
	defer smtpServer.mu.Unlock()
	if len(smtpServer.to) != 1 || smtpServer.to[0] != "alice.42@example.com" {
		t.Fatalf("Exchange accepted recipients = %v", smtpServer.to)
	}
	if !strings.Contains(smtpServer.data.String(), "secret body") {
		t.Fatal("Exchange did not receive the message for the accepted recipient")
	}
}

func TestHandleMessagePartialRecipientRejectionDeletesWhenDSNFails(t *testing.T) {
	smtpServer := newFakeSMTPServer(t, map[string]string{
		"missing.42@example.com": "550 5.1.1 mailbox unavailable",
	})
	defer smtpServer.Close()

	host, portString, found := strings.Cut(smtpServer.addr, ":")
	if !found || host == "" {
		t.Fatalf("split SMTP address %q", smtpServer.addr)
	}

	sqsClient := &fakeWorkerSQS{}
	s3Client := &fakeWorkerS3{raw: []byte(
		"From: sender@external.example\r\nSubject: original\r\n\r\nsecret body\r\n")}
	sesClient := &fakeWorkerSES{err: errors.New("SES throttling")}
	w := newTestWorker(t, sqsClient, s3Client, sesClient, host, atoi(t, portString))

	w.handleMessage(context.Background(), notificationMessage(t,
		"sender@external.example",
		[]string{"missing.42@example.com", "alice.42@example.com"}))

	if len(sesClient.inputs) != 1 {
		t.Fatalf("SES SendEmail calls = %d, want 1", len(sesClient.inputs))
	}
	if sqsClient.deleteCalls != 1 {
		t.Fatalf("SQS DeleteMessage calls = %d, want 1 after partial delivery", sqsClient.deleteCalls)
	}

	smtpServer.mu.Lock()
	defer smtpServer.mu.Unlock()
	if len(smtpServer.to) != 1 || smtpServer.to[0] != "alice.42@example.com" {
		t.Fatalf("Exchange accepted recipients = %v", smtpServer.to)
	}
	if !strings.Contains(smtpServer.data.String(), "secret body") {
		t.Fatal("Exchange did not receive the message for the accepted recipient")
	}
}

func TestHandleMessageAllRecipientsRejectedRedrivesWhenDSNFails(t *testing.T) {
	smtpServer := newFakeSMTPServer(t, map[string]string{
		"missing.42@example.com": "550 5.1.1 mailbox unavailable",
	})
	defer smtpServer.Close()

	host, portString, found := strings.Cut(smtpServer.addr, ":")
	if !found || host == "" {
		t.Fatalf("split SMTP address %q", smtpServer.addr)
	}

	sqsClient := &fakeWorkerSQS{}
	s3Client := &fakeWorkerS3{raw: []byte(
		"From: sender@external.example\r\nSubject: original\r\n\r\nsecret body\r\n")}
	sesClient := &fakeWorkerSES{err: errors.New("SES throttling")}
	w := newTestWorker(t, sqsClient, s3Client, sesClient, host, atoi(t, portString))

	w.handleMessage(context.Background(), notificationMessage(t,
		"sender@external.example", []string{"missing.42@example.com"}))

	if len(sesClient.inputs) != 1 {
		t.Fatalf("SES SendEmail calls = %d, want 1", len(sesClient.inputs))
	}
	if sqsClient.deleteCalls != 0 {
		t.Fatalf("SQS DeleteMessage calls = %d, want 0 when nothing was delivered", sqsClient.deleteCalls)
	}
}

func newTestWorker(
	t *testing.T,
	sqsClient *fakeWorkerSQS,
	s3Client *fakeWorkerS3,
	sesClient *fakeWorkerSES,
	exchangeHost string,
	exchangePort int,
) *Worker {
	t.Helper()
	return &Worker{
		cfg: config.InboundConfig{
			SQSQueueURL:     "https://sqs.us-east-1.amazonaws.com/123456789012/tca-mail-inbound-42",
			RecipientSuffix: ".42@example.com",
			BounceSender:    "postmaster.42@example.com",
			Exchange: config.ExchangeConfig{
				Host:       exchangeHost,
				Port:       exchangePort,
				HeloDomain: "test.local",
			},
		},
		clients: workerClients{SQS: sqsClient, S3: s3Client, SES: sesClient},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func notificationMessage(t *testing.T, source string, destinations []string) sqstypes.Message {
	t.Helper()
	notification := map[string]any{
		"notificationType": "Received",
		"mail": map[string]any{
			"messageId":   "ses-message-id",
			"source":      source,
			"destination": destinations,
		},
		"receipt": map[string]any{
			"action": map[string]any{
				"type":       "S3",
				"bucketName": "tca-mail-inbound-test",
				"objectKey":  "raw/message-id",
			},
		},
	}
	body, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	return sqstypes.Message{
		MessageId:     aws.String("sqs-message-id"),
		ReceiptHandle: aws.String("receipt-handle"),
		Body:          aws.String(string(body)),
	}
}

func TestBuildRejectionDSN(t *testing.T) {
	raw, err := buildRejectionDSN(
		"postmaster.42@example.com",
		"sender@external.example",
		"relay.example.com",
		"ses-message-id",
		[]recipientFailure{{
			Recipient:  "missing.42@example.com",
			Diagnostic: "550 5.1.1 mailbox unavailable",
		}},
		[]byte("From: sender@external.example\r\nTo: missing.42@example.com\r\nSubject: original\r\n\r\nsecret body\r\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Content-Type: multipart/report; report-type=delivery-status",
		"Auto-Submitted: auto-replied",
		"Reporting-MTA: dns; relay.example.com",
		"Final-Recipient: rfc822; missing.42@example.com",
		"Action: failed",
		"Status: 5.1.1",
		"Diagnostic-Code: smtp; 550 5.1.1 mailbox unavailable",
		"Subject: original",
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("DSN missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "secret body") {
		t.Fatal("DSN included original message body")
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if _, err := mail.ParseDate(message.Header.Get("Date")); err != nil {
		t.Fatalf("DSN Date header is missing or invalid: %v", err)
	}
}

func TestShouldBounceLoopGuard(t *testing.T) {
	for _, sender := range []string{"", "postmaster@example.com", "postmaster.42@example.com", "bad address"} {
		if shouldBounce(sender, []byte("Subject: test\r\n\r\nbody")) {
			t.Errorf("shouldBounce(%q) = true", sender)
		}
	}
	if !shouldBounce("sender@external.example", []byte("Subject: test\r\n\r\nbody")) {
		t.Fatal("ordinary sender should be bounceable")
	}
	if !shouldBounce("sender@external.example", []byte("Auto-Submitted: no\r\n\r\nbody")) {
		t.Fatal("Auto-Submitted: no should be bounceable")
	}
	for _, value := range []string{"auto-replied", "auto-generated; owner-email=owner@example.com"} {
		raw := []byte("Auto-Submitted: " + value + "\r\n\r\nbody")
		if shouldBounce("sender@external.example", raw) {
			t.Errorf("Auto-Submitted: %s should not be bounceable", value)
		}
	}
}

func TestEnhancedStatusPatternSupportsMultiDigitSubfields(t *testing.T) {
	for _, test := range []struct {
		diagnostic string
		want       string
	}{
		{diagnostic: "550 5.1.1 mailbox unavailable", want: "5.1.1"},
		{diagnostic: "550 5.7.26 unauthenticated email", want: "5.7.26"},
		{diagnostic: "550 5.7.509 access denied", want: "5.7.509"},
	} {
		match := enhancedStatusPattern.FindStringSubmatch(test.diagnostic)
		if len(match) != 2 || match[1] != test.want {
			t.Errorf("enhanced status in %q = %v, want %q", test.diagnostic, match, test.want)
		}
	}
}

func TestReportingMTADomainFallsBackToBounceSenderDomain(t *testing.T) {
	if got := reportingMTADomain("", "postmaster.42@example.com"); got != "example.com" {
		t.Fatalf("reporting MTA domain = %q, want example.com", got)
	}
}
