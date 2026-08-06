// Package inbound implements the SQS->SMTP relay direction: it long-polls an
// SQS queue subscribed to SES receipt notifications, fetches each raw MIME
// message from S3, and forwards it to the on-premise Exchange server over
// SMTP.
package inbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/awsclient"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// Worker polls an SQS queue for SES receipt notifications and relays the
// referenced MIME messages to Exchange.
type Worker struct {
	cfg     config.InboundConfig
	clients workerClients
	log     *slog.Logger
}

type sqsClient interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type s3Client interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type sesClient interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

type workerClients struct {
	SQS sqsClient
	S3  s3Client
	SES sesClient
}

// New constructs a Worker. Run starts the polling loop.
func New(cfg config.InboundConfig, clients *awsclient.Clients, log *slog.Logger) *Worker {
	return &Worker{
		cfg: cfg,
		clients: workerClients{
			SQS: clients.SQS,
			S3:  clients.S3,
			SES: clients.SES,
		},
		log: log.With("component", "inbound"),
	}
}

// Run polls SQS until ctx is cancelled. It returns ctx.Err() on graceful
// shutdown and any unrecoverable error otherwise.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("starting inbound worker",
		"queueUrl", w.cfg.SQSQueueURL,
		"maxConcurrent", w.cfg.MaxConcurrent,
		"exchange", fmt.Sprintf("%s:%d", w.cfg.Exchange.Host, w.cfg.Exchange.Port),
	)

	sem := make(chan struct{}, w.cfg.MaxConcurrent)
	var wg sync.WaitGroup

	for {
		if err := ctx.Err(); err != nil {
			wg.Wait()
			return err
		}

		out, err := w.clients.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(w.cfg.SQSQueueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     int32(w.cfg.PollWaitSeconds),
			VisibilityTimeout:   int32(w.cfg.VisibilityTimeoutSeconds),
		})
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return ctx.Err()
			}
			w.log.Error("sqs receive failed", "err", err)
			// Don't tight-loop on persistent errors.
			if !sleepWithCtx(ctx, 5) {
				wg.Wait()
				return ctx.Err()
			}
			continue
		}

		for _, msg := range out.Messages {
			select {
			case <-ctx.Done():
				wg.Wait()
				return ctx.Err()
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(m sqstypes.Message) {
				defer wg.Done()
				defer func() { <-sem }()
				w.handleMessage(ctx, m)
			}(msg)
		}
	}
}

func (w *Worker) handleMessage(ctx context.Context, msg sqstypes.Message) {
	mid := aws.ToString(msg.MessageId)
	log := w.log.With("sqsMessageId", mid)

	body := []byte(aws.ToString(msg.Body))
	notif, err := parseNotification(body)
	if err != nil {
		log.Error("could not parse SES notification - leaving message for redrive", "err", err)
		return
	}
	if notif.Receipt.Action.Type != "S3" {
		log.Error("notification action is not S3, skipping",
			"actionType", notif.Receipt.Action.Type)
		// Permanent error - delete so it doesn't keep redriving.
		w.deleteMessage(ctx, log, msg)
		return
	}
	if !recipientsMatchSuffix(notif.Mail.Destination, w.cfg.RecipientSuffix) {
		log.Error("notification contains a recipient outside the configured suffix - dropping message",
			"recipientSuffix", w.cfg.RecipientSuffix,
			"recipients", notif.Mail.Destination)
		w.deleteMessage(ctx, log, msg)
		return
	}

	bucket := notif.Receipt.Action.BucketName
	if w.cfg.S3Bucket != "" && bucket != w.cfg.S3Bucket {
		log.Error("notification refers to unexpected S3 bucket - leaving for redrive",
			"expected", w.cfg.S3Bucket, "got", bucket)
		return
	}
	key := notif.Receipt.Action.ObjectKey

	log = log.With(
		"emailMessageId", notif.Mail.MessageID,
		"s3Bucket", bucket,
		"s3Key", key,
		"from", notif.Mail.Source,
		"to", notif.Mail.Destination,
	)

	raw, err := w.fetchS3Object(ctx, bucket, key)
	if err != nil {
		log.Error("fetch S3 object failed - leaving message for redrive", "err", err)
		return
	}
	log.Debug("fetched raw email", "bytes", len(raw))

	if err := relayToExchange(w.cfg.Exchange, notif.Mail.Source, notif.Mail.Destination, raw); err != nil {
		if IsPermanent(err) {
			failures := rejectedRecipients(err)
			if len(failures) == 0 {
				failures = make([]recipientFailure, 0, len(notif.Mail.Destination))
				for _, recipient := range notif.Mail.Destination {
					failures = append(failures, recipientFailure{
						Recipient:  recipient,
						Diagnostic: err.Error(),
					})
				}
			}
			if shouldBounce(notif.Mail.Source) && w.cfg.BounceSender != "" {
				if bounceErr := w.sendRejectionDSN(ctx, notif, raw, failures); bounceErr != nil {
					log.Error("permanent SMTP failure and rejection DSN failed - leaving message for redrive",
						"err", err, "bounceErr", bounceErr)
					return
				}
				log.Info("sent rejection DSN", "rejectedRecipients", len(failures))
			} else {
				log.Warn("permanent SMTP failure cannot be bounced", "err", err)
			}
			w.deleteMessage(ctx, log, msg)
			return
		}
		log.Warn("transient SMTP failure - leaving message for redrive", "err", err)
		return
	}

	log.Info("relayed email to Exchange")
	w.deleteMessage(ctx, log, msg)
}

func recipientsMatchSuffix(recipients []string, suffix string) bool {
	if suffix == "" {
		return true
	}
	if len(recipients) == 0 {
		return false
	}
	suffix = strings.ToLower(suffix)
	for _, recipient := range recipients {
		parsed, err := mail.ParseAddress(recipient)
		if err != nil || parsed.Address != recipient ||
			!strings.HasSuffix(strings.ToLower(parsed.Address), suffix) {
			return false
		}
	}
	return true
}

func shouldBounce(sender string) bool {
	if sender == "" {
		return false
	}
	parsed, err := mail.ParseAddress(sender)
	if err != nil || parsed.Address != sender {
		return false
	}
	local, _, ok := strings.Cut(strings.ToLower(parsed.Address), "@")
	return ok && !strings.HasPrefix(local, "postmaster")
}

func (w *Worker) sendRejectionDSN(
	ctx context.Context,
	notif *sesNotification,
	original []byte,
	failures []recipientFailure,
) error {
	raw, err := buildRejectionDSN(w.cfg.BounceSender, notif.Mail.Source, notif.Mail.MessageID, failures, original)
	if err != nil {
		return err
	}
	_, err = w.clients.SES.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(w.cfg.BounceSender),
		Destination:      &types.Destination{ToAddresses: []string{notif.Mail.Source}},
		Content:          &types.EmailContent{Raw: &types.RawMessage{Data: raw}},
	})
	return err
}

var enhancedStatusPattern = regexp.MustCompile(`\b(5\.\d\.\d)\b`)

func buildRejectionDSN(
	from string,
	to string,
	originalMessageID string,
	failures []recipientFailure,
	original []byte,
) ([]byte, error) {
	var body bytes.Buffer
	multi := multipart.NewWriter(&body)

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textPart, err := multi.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(textPart,
		"Your message could not be delivered to one or more recipients.\r\n\r\n")
	for _, failure := range failures {
		_, _ = fmt.Fprintf(textPart, "  %s: %s\r\n", failure.Recipient, sanitizeDSNField(failure.Diagnostic))
	}

	statusHeader := textproto.MIMEHeader{}
	statusHeader.Set("Content-Type", "message/delivery-status")
	statusPart, err := multi.CreatePart(statusHeader)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(statusPart, "Reporting-MTA: dns; ses-smtp-proxy\r\n")
	if originalMessageID != "" {
		_, _ = fmt.Fprintf(statusPart, "Original-Envelope-Id: %s\r\n", sanitizeDSNField(originalMessageID))
	}
	for _, failure := range failures {
		status := "5.0.0"
		if match := enhancedStatusPattern.FindStringSubmatch(failure.Diagnostic); len(match) == 2 {
			status = match[1]
		}
		_, _ = fmt.Fprintf(statusPart,
			"\r\nFinal-Recipient: rfc822; %s\r\nAction: failed\r\nStatus: %s\r\nDiagnostic-Code: smtp; %s\r\n",
			sanitizeDSNField(failure.Recipient), status, sanitizeDSNField(failure.Diagnostic))
	}

	headersPartHeader := textproto.MIMEHeader{}
	headersPartHeader.Set("Content-Type", "message/rfc822-headers")
	headersPart, err := multi.CreatePart(headersPartHeader)
	if err != nil {
		return nil, err
	}
	_, _ = headersPart.Write(originalHeaders(original))
	if err := multi.Close(); err != nil {
		return nil, err
	}

	var message bytes.Buffer
	_, _ = fmt.Fprintf(&message,
		"From: Mail Delivery Subsystem <%s>\r\nTo: <%s>\r\nSubject: Delivery Status Notification (Failure)\r\nAuto-Submitted: auto-replied\r\nMIME-Version: 1.0\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=%q\r\n\r\n",
		from, to, multi.Boundary())
	_, _ = message.Write(body.Bytes())
	return message.Bytes(), nil
}

func originalHeaders(raw []byte) []byte {
	if index := bytes.Index(raw, []byte("\r\n\r\n")); index >= 0 {
		return raw[:index+2]
	}
	if index := bytes.Index(raw, []byte("\n\n")); index >= 0 {
		return raw[:index+1]
	}
	return raw
}

func sanitizeDSNField(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func (w *Worker) fetchS3Object(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := w.clients.S3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, out.Body); err != nil {
		return nil, fmt.Errorf("read S3 body: %w", err)
	}
	return buf.Bytes(), nil
}

func (w *Worker) deleteMessage(ctx context.Context, log *slog.Logger, msg sqstypes.Message) {
	_, err := w.clients.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(w.cfg.SQSQueueURL),
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Warn("sqs delete failed", "err", err)
	}
}

// sleepWithCtx sleeps for seconds. Returns false if the context is cancelled
// during the sleep.
func sleepWithCtx(ctx context.Context, seconds int) bool {
	t := newTimer(seconds)
	defer t.stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.c:
		return true
	}
}
