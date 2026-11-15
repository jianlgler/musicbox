// Package storage is the object storage layer: audio bytes in an S3 bucket.
// It speaks the S3 API, so the same code works with SeaweedFS, AWS S3, R2 or B2.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ErrNotFound is returned when an object doesn't exist, so callers never need to know S3 error codes
var ErrNotFound = errors.New("object not found")

type Config struct {
	Endpoint  string // e.g. http://127.0.0.1:8333
	Bucket    string
	AccessKey string
	SecretKey string // never logged, never part of an error message
}

type Store struct {
	client *s3.Client
	bucket string
}

// New only builds the client and doesn't connect. Ping checks the connection.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("[STORAGE] endpoint, bucket, access key and secret key are all required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// %q and not the parse error: the endpoint is config, but keep the message one clean line
		return nil, fmt.Errorf("[STORAGE] endpoint must be an http(s) URL, got %q", cfg.Endpoint)
	}

	creds := aws.Credentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey, Source: "musicbox config"}

	// Built by hand instead of with the SDK's config package: that one also reads ~/.aws,
	// AWS_* variables and the EC2 metadata service. Here every setting is explicit.
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       "us-east-1", // SeaweedFS ignores it, but SigV4 signs it, so it must be set
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return creds, nil
		}),
		// Path style (http://host/bucket/key) instead of virtual-host style (http://bucket.host/key),
		// which would need a DNS name per bucket
		UsePathStyle: true,
	})
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// Ping checks that storage is reachable, the keys are right and the bucket exists
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("[STORAGE] head bucket: %w", err)
	}
	return nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AudioKey turns a file's SHA-256 into its object key. Content-addressed: same file, same key.
// Rejecting anything that isn't 64 lowercase hex characters keeps keys predictable,
// so input can never become "audio/../other" or a key outside audio/.
func AudioKey(sha256 string) (string, error) {
	if !sha256Hex.MatchString(sha256) {
		return "", fmt.Errorf("[STORAGE] not a lowercase hex SHA-256: %q", sha256)
	}
	return "audio/" + sha256, nil
}

// Put stores size bytes from body under key. Objects are never modified in place, only
// replaced whole. size must be exact: S3 needs Content-Length up front for a streamed body.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("[STORAGE] put %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is stored, without downloading it. Used to skip duplicate uploads.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("[STORAGE] head %q: %w", key, err)
}

// Get opens the object for reading. The caller must Close it, or the connection leaks.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("[STORAGE] get %q: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes key. Deleting a key that doesn't exist is not an error (S3 semantics),
// which makes cleanup jobs safe to retry.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("[STORAGE] delete %q: %w", key, err)
	}
	return nil
}

// isNotFound matches both forms: GetObject returns NoSuchKey (it has a body to carry the code),
// HeadObject returns a bare 404 the SDK reports as NotFound (HEAD responses have no body).
func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
