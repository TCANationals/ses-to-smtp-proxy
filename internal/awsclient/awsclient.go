// Package awsclient constructs AWS SDK v2 service clients shared by the
// inbound and outbound workers.
//
// Credentials are resolved in this order:
//
//  1. Static credentials from the config file (aws.accessKeyId / aws.secretAccessKey)
//  2. A named shared-config profile (aws.profile)
//  3. The default credential chain (environment variables, EC2/ECS roles,
//     shared credentials file, etc.)
package awsclient

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
)

// Clients bundles the AWS service clients used by the proxy.
type Clients struct {
	SQS *sqs.Client
	S3  *s3.Client
	SES *sesv2.Client
}

// New loads an AWS SDK config according to the package documentation and
// constructs the SQS, S3, and SES service clients.
func New(ctx context.Context, cfg config.AWSConfig) (*Clients, error) {
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{
		SQS: sqs.NewFromConfig(awsCfg),
		S3:  s3.NewFromConfig(awsCfg),
		SES: sesv2.NewFromConfig(awsCfg),
	}, nil
}

func loadAWSConfig(ctx context.Context, cfg config.AWSConfig) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}

	switch {
	case cfg.AccessKeyID != "" && cfg.SecretAccessKey != "":
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	case cfg.Profile != "":
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load aws config: %w", err)
	}
	return awsCfg, nil
}
