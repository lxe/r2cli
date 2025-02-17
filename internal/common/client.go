package common

import (
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2URL struct {
	Endpoint string
	Bucket   string
	Key      string
}

func ParseR2URL(r2url string) (*R2URL, error) {
	if !strings.HasPrefix(r2url, "r2://") {
		return nil, fmt.Errorf("invalid R2 URL format, must start with r2://")
	}

	// Remove r2:// prefix
	urlStr := strings.TrimPrefix(r2url, "r2://")

	// Split into parts
	parts := strings.SplitN(urlStr, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid R2 URL format, must be r2://<endpoint>/<bucket>/<key>")
	}

	endpoint := parts[0]
	bucketAndKey := strings.SplitN(parts[1], "/", 2)
	if len(bucketAndKey) != 2 {
		return nil, fmt.Errorf("invalid R2 URL format, missing key")
	}

	return &R2URL{
		Endpoint: fmt.Sprintf("https://%s", endpoint),
		Bucket:   bucketAndKey[0],
		Key:      bucketAndKey[1],
	}, nil
}

func getCredentials() (string, string, error) {
	// Try R2-prefixed variables first
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")

	// Fall back to non-prefixed variables if R2-prefixed ones are not set
	if accessKey == "" || secretKey == "" {
		accessKey = os.Getenv("ACCESS_KEY_ID")
		secretKey = os.Getenv("SECRET_ACCESS_KEY")
	}

	if accessKey == "" || secretKey == "" {
		return "", "", fmt.Errorf("neither R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY nor ACCESS_KEY_ID/SECRET_ACCESS_KEY environment variables are set")
	}

	return accessKey, secretKey, nil
}

func NewR2Client(endpoint string) (*s3.Client, error) {
	accessKey, secretKey, err := getCredentials()
	if err != nil {
		return nil, err
	}

	creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""))

	client := s3.New(s3.Options{
		Region:           "auto",
		Credentials:      creds,
		EndpointResolver: s3.EndpointResolverFromURL(endpoint),
		UsePathStyle:     true,
	})

	return client, nil
}
