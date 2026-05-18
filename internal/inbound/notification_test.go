package inbound

import (
	"encoding/json"
	"testing"
)

const rawSESNotification = `{
  "notificationType": "Received",
  "receipt": {
    "timestamp": "2026-05-18T16:00:00.000Z",
    "recipients": ["alice@example.com"],
    "action": {
      "type": "S3",
      "topicArn": "arn:aws:sns:us-east-1:123456789012:ses-smtp-proxy",
      "bucketName": "ses-smtp-proxy-inbound",
      "objectKey": "emails/abc123"
    }
  },
  "mail": {
    "messageId": "abc123",
    "source": "bob@external.example",
    "destination": ["alice@example.com"]
  }
}`

func TestParseNotificationRaw(t *testing.T) {
	n, err := parseNotification([]byte(rawSESNotification))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.NotificationType != "Received" {
		t.Errorf("type: %q", n.NotificationType)
	}
	if n.Receipt.Action.BucketName != "ses-smtp-proxy-inbound" {
		t.Errorf("bucket: %q", n.Receipt.Action.BucketName)
	}
	if n.Receipt.Action.ObjectKey != "emails/abc123" {
		t.Errorf("key: %q", n.Receipt.Action.ObjectKey)
	}
	if n.Mail.Source != "bob@external.example" {
		t.Errorf("source: %q", n.Mail.Source)
	}
	if len(n.Mail.Destination) != 1 || n.Mail.Destination[0] != "alice@example.com" {
		t.Errorf("destination: %v", n.Mail.Destination)
	}
}

func TestParseNotificationSNSWrapped(t *testing.T) {
	envelope := map[string]string{
		"Type":      "Notification",
		"MessageId": "sns-msg-1",
		"TopicArn":  "arn:aws:sns:us-east-1:123456789012:ses-smtp-proxy",
		"Message":   rawSESNotification,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	n, err := parseNotification(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Receipt.Action.ObjectKey != "emails/abc123" {
		t.Errorf("key: %q", n.Receipt.Action.ObjectKey)
	}
}

func TestParseNotificationInvalid(t *testing.T) {
	cases := []string{
		``,
		`{`,
		`{"Type":"SubscriptionConfirmation","Message":"hello"}`,
		`{"Type":"Notification","Message":"not json"}`,
	}
	for _, body := range cases {
		if _, err := parseNotification([]byte(body)); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}
