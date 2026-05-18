package outbound

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
)

// sesSender abstracts the subset of the SES client we use, for testability.
type sesSender interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// sendResult describes the outcome of a single SES SendEmail call.
type sendResult struct {
	messageID string
	err       error
	permanent bool // true if the error is non-retryable
}

func sendViaSES(ctx context.Context, client sesSender, from string, to []string, raw []byte) sendResult {
	out, err := client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from),
		Destination: &types.Destination{
			ToAddresses: to,
		},
		Content: &types.EmailContent{
			Raw: &types.RawMessage{Data: raw},
		},
	})
	if err != nil {
		return sendResult{err: err, permanent: isPermanentSESError(err)}
	}
	return sendResult{messageID: aws.ToString(out.MessageId)}
}

// isPermanentSESError reports whether err from SES is a permanent failure
// (e.g. configuration / identity issues) versus a transient one (throttling,
// network, etc.).
func isPermanentSESError(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case
		"MailFromDomainNotVerifiedException",
		"MessageRejected",
		"ConfigurationSetDoesNotExistException",
		"AccountSuspendedException",
		"SendingPausedException",
		"ConfigurationSetSendingPausedException",
		"NotFoundException":
		return true
	}
	// Fault classification: client-side faults are typically permanent.
	if apiErr.ErrorFault() == smithy.FaultClient {
		// Throttling is client-side per smithy but should be retried.
		if apiErr.ErrorCode() == "ThrottlingException" || apiErr.ErrorCode() == "TooManyRequestsException" {
			return false
		}
		return true
	}
	return false
}

// describeSESError returns a one-line description suitable for SMTP response
// text. SES error messages can be long; we cap and sanitize.
func describeSESError(err error) string {
	const maxLen = 200
	s := err.Error()
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	for i, r := range s {
		if r < 0x20 || r > 0x7e {
			s = s[:i]
			break
		}
	}
	if s == "" {
		return "SES request failed"
	}
	return fmt.Sprintf("SES: %s", s)
}
