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
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/awsclient"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// Worker polls an SQS queue for SES receipt notifications and relays the
// referenced MIME messages to Exchange.
type Worker struct {
	cfg     config.InboundConfig
	clients *awsclient.Clients
	log     *slog.Logger
}

// New constructs a Worker. Run starts the polling loop.
func New(cfg config.InboundConfig, clients *awsclient.Clients, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, clients: clients, log: log.With("component", "inbound")}
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
			log.Error("permanent SMTP failure - dropping message", "err", err)
			w.deleteMessage(ctx, log, msg)
			return
		}
		log.Warn("transient SMTP failure - leaving message for redrive", "err", err)
		return
	}

	log.Info("relayed email to Exchange")
	w.deleteMessage(ctx, log, msg)
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
