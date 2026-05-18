package inbound

import (
	"encoding/json"
	"fmt"
)

// sesNotification is the subset of the SES receipt notification we care
// about. SES publishes this to the SNS topic configured on the receipt rule's
// S3 action, and we receive it as the SQS message body when the SNS->SQS
// subscription has RawMessageDelivery enabled.
//
// Reference:
// https://docs.aws.amazon.com/ses/latest/dg/receiving-email-notifications-contents.html
type sesNotification struct {
	NotificationType string `json:"notificationType"`
	Receipt          struct {
		Action struct {
			Type       string `json:"type"`
			BucketName string `json:"bucketName"`
			ObjectKey  string `json:"objectKey"`
		} `json:"action"`
	} `json:"receipt"`
	Mail struct {
		MessageID   string   `json:"messageId"`
		Source      string   `json:"source"`
		Destination []string `json:"destination"`
	} `json:"mail"`
}

// snsEnvelope wraps an SES notification when the SNS->SQS subscription does
// not have RawMessageDelivery enabled. We handle both shapes defensively.
type snsEnvelope struct {
	Type      string `json:"Type"`
	MessageID string `json:"MessageId"`
	TopicArn  string `json:"TopicArn"`
	Subject   string `json:"Subject"`
	Message   string `json:"Message"`
}

// parseNotification accepts an SQS body and returns the embedded SES
// notification regardless of whether SNS wrapped it or not.
func parseNotification(body []byte) (*sesNotification, error) {
	// First, try parsing as a raw SES notification.
	var raw sesNotification
	if err := json.Unmarshal(body, &raw); err == nil && raw.NotificationType != "" {
		return &raw, nil
	}

	// Fall back to SNS-wrapped envelope.
	var env snsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if env.Type != "Notification" || env.Message == "" {
		return nil, fmt.Errorf("not an SES notification (Type=%q)", env.Type)
	}
	var inner sesNotification
	if err := json.Unmarshal([]byte(env.Message), &inner); err != nil {
		return nil, fmt.Errorf("parse inner SES notification: %w", err)
	}
	if inner.NotificationType == "" {
		return nil, fmt.Errorf("inner message is not an SES notification")
	}
	return &inner, nil
}
