package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/robertpitt/git3/internal/config"
	"github.com/robertpitt/git3/internal/locator"
)

// S3 implements Store using Amazon S3-compatible APIs.
type S3 struct {
	client     s3API
	bucket     string
	loc        locator.Locator
	cfg        config.Config
	encryption types.ServerSideEncryption
}

type s3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// NewS3 creates an S3-backed store for l using c.
func NewS3(ctx context.Context, l locator.Locator, c config.Config) (*S3, error) {
	ac, e := loadAWSConfig(ctx, c)
	if e != nil {
		return nil, e
	}
	cl := s3.NewFromConfig(ac, func(o *s3.Options) {
		o.UsePathStyle = c.PathStyle
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	})
	x := &S3{client: cl, bucket: l.Bucket, loc: l, cfg: c}
	if c.SSE == "s3" {
		x.encryption = types.ServerSideEncryptionAes256
	} else if c.SSE == "kms" {
		x.encryption = types.ServerSideEncryptionAwsKms
	}
	return x, nil
}

func loadAWSConfig(ctx context.Context, c config.Config) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	opts = append(opts, awsconfig.WithRetryMaxAttempts(c.MaxAttempts))
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}
func (s *S3) key(k string) string { return s.loc.Key(k) }
func mapS3(err error) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return ErrNotFound
		case "NotModified", "304":
			return ErrNotModified
		case "PreconditionFailed", "412", "ConditionalRequestConflict", "409":
			return ErrPrecondition
		case "AccessDenied", "InvalidAccessKeyId", "ExpiredToken":
			return fmt.Errorf("authorization: %w", err)
		case "NoSuchBucket":
			return fmt.Errorf("bucket not found: %w", err)
		}
	}
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		switch hs.HTTPStatusCode() {
		case 304:
			return ErrNotModified
		case 404:
			return ErrNotFound
		case 409, 412:
			return ErrPrecondition
		case 401, 403:
			return fmt.Errorf("authorization: %w", err)
		}
	}
	return err
}

// Get returns the current object or validates a conditional ETag.
func (s *S3) Get(ctx context.Context, key, none string) (Object, error) {
	in := &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))}
	if none != "" {
		in.IfNoneMatch = aws.String(none)
	}
	out, e := s.client.GetObject(ctx, in)
	if e != nil {
		return Object{}, mapS3(e)
	}
	defer out.Body.Close()
	limit := getLimit(key)
	if out.ContentLength != nil && *out.ContentLength > limit {
		return Object{}, fmt.Errorf("object %s exceeds read limit", key)
	}
	b, e := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if int64(len(b)) > limit {
		return Object{}, fmt.Errorf("object %s exceeds read limit", key)
	}
	if e != nil {
		return Object{}, e
	}
	return Object{Body: b, ETag: aws.ToString(out.ETag), Size: int64(len(b)), LastModified: aws.ToTime(out.LastModified)}, nil
}
func getLimit(key string) int64 {
	switch {
	case key == ".git/git3/HEAD":
		return 2 << 20
	case strings.HasPrefix(key, ".git/git3/transactions/"):
		return 64 << 20
	case strings.HasPrefix(key, ".git/git3/log-pages/"), strings.HasPrefix(key, ".git/git3/refs/"):
		return 128 << 20
	case strings.HasPrefix(key, ".git/git3/packsets/"):
		return 64 << 20
	case strings.HasPrefix(key, ".git/git3/gc/"):
		return 256 << 20
	default:
		return 1 << 40
	}
}

// GetRange returns the inclusive byte range [start, end].
func (s *S3) GetRange(ctx context.Context, key string, start, end int64) ([]byte, error) {
	out, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), Range: aws.String(fmt.Sprintf("bytes=%d-%d", start, end))})
	if e != nil {
		return nil, mapS3(e)
	}
	defer out.Body.Close()
	want := end - start + 1
	b, e := io.ReadAll(io.LimitReader(out.Body, want+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > want {
		return nil, fmt.Errorf("range response too large")
	}
	return b, nil
}

// Head returns metadata for the current object.
func (s *S3) Head(ctx context.Context, key string) (Metadata, error) {
	out, e := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))})
	if e != nil {
		return Metadata{}, mapS3(e)
	}
	return Metadata{Key: key, ETag: aws.ToString(out.ETag), Size: aws.ToInt64(out.ContentLength), LastModified: aws.ToTime(out.LastModified)}, nil
}

// Put stores an object when its supplied preconditions hold.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) (Metadata, error) {
	if size < 0 {
		prefix, e := io.ReadAll(io.LimitReader(r, s.cfg.MultipartThreshold+1))
		if e != nil {
			return Metadata{}, e
		}
		if int64(len(prefix)) <= s.cfg.MultipartThreshold {
			return s.putSingle(ctx, key, bytes.NewReader(prefix), int64(len(prefix)), o)
		}
		return s.putMultipart(ctx, key, io.MultiReader(bytes.NewReader(prefix), r), o)
	}
	if size >= s.cfg.MultipartThreshold && o.IfMatch == "" {
		return s.putMultipart(ctx, key, r, o)
	}
	return s.putSingle(ctx, key, r, size, o)
}
func (s *S3) putSingle(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) (Metadata, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), Body: r, ChecksumAlgorithm: types.ChecksumAlgorithmSha256}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if o.IfMatch != "" {
		in.IfMatch = aws.String(o.IfMatch)
	}
	if o.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if s.encryption != "" {
		in.ServerSideEncryption = s.encryption
	}
	if s.encryption == types.ServerSideEncryptionAwsKms {
		if s.cfg.KMSKeyID != "" {
			in.SSEKMSKeyId = aws.String(s.cfg.KMSKeyID)
		}
		in.BucketKeyEnabled = s.cfg.BucketKeyEnabled
	}
	if o.ContentSHA256 != "" {
		if b, e := decodeHex(o.ContentSHA256); e == nil {
			in.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(b))
		}
	}
	out, e := s.client.PutObject(ctx, in)
	if e != nil {
		return Metadata{}, mapS3(e)
	}
	return Metadata{Key: key, ETag: aws.ToString(out.ETag), Size: size, LastModified: time.Now().UTC()}, nil
}
func (s *S3) putMultipart(ctx context.Context, key string, r io.Reader, o PutOptions) (Metadata, error) {
	create := &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), ChecksumAlgorithm: types.ChecksumAlgorithmSha256}
	if s.encryption != "" {
		create.ServerSideEncryption = s.encryption
	}
	if s.encryption == types.ServerSideEncryptionAwsKms {
		if s.cfg.KMSKeyID != "" {
			create.SSEKMSKeyId = aws.String(s.cfg.KMSKeyID)
		}
		create.BucketKeyEnabled = s.cfg.BucketKeyEnabled
	}
	started, e := s.client.CreateMultipartUpload(ctx, create)
	if e != nil {
		return Metadata{}, mapS3(e)
	}
	done := false
	defer func() {
		if !done {
			aCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = s.client.AbortMultipartUpload(aCtx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), UploadId: started.UploadId})
		}
	}()
	if s.cfg.PartSize > int64(^uint(0)>>1) {
		return Metadata{}, fmt.Errorf("part size exceeds platform limit")
	}
	buf := make([]byte, int(s.cfg.PartSize))
	var parts []types.CompletedPart
	var total int64
	for nPart := int32(1); ; nPart++ {
		n, er := io.ReadFull(r, buf)
		if er == io.EOF && n == 0 {
			break
		}
		if er != nil && er != io.ErrUnexpectedEOF {
			return Metadata{}, er
		}
		body := bytes.NewReader(buf[:n])
		up, e := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), UploadId: started.UploadId, PartNumber: aws.Int32(nPart), Body: body, ContentLength: aws.Int64(int64(n)), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
		if e != nil {
			return Metadata{}, mapS3(e)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(nPart), ChecksumSHA256: up.ChecksumSHA256})
		total += int64(n)
		if er == io.ErrUnexpectedEOF {
			break
		}
		if nPart >= 10000 {
			return Metadata{}, fmt.Errorf("multipart upload exceeds 10,000 parts")
		}
	}
	complete := &s3.CompleteMultipartUploadInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), UploadId: started.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}
	if o.IfMatch != "" {
		complete.IfMatch = aws.String(o.IfMatch)
	}
	if o.IfNoneMatch {
		complete.IfNoneMatch = aws.String("*")
	}
	out, e := s.client.CompleteMultipartUpload(ctx, complete)
	if e != nil {
		return Metadata{}, mapS3(e)
	}
	done = true
	return Metadata{Key: key, ETag: aws.ToString(out.ETag), Size: total, LastModified: time.Now().UTC()}, nil
}
func decodeHex(v string) ([]byte, error) { return bytesToHex(v) }
func bytesToHex(v string) ([]byte, error) {
	const h = "0123456789abcdef"
	if len(v)%2 != 0 {
		return nil, fmt.Errorf("hex")
	}
	b := make([]byte, len(v)/2)
	for i := range b {
		a := strings.IndexByte(h, v[i*2])
		c := strings.IndexByte(h, v[i*2+1])
		if a < 0 || c < 0 {
			return nil, fmt.Errorf("hex")
		}
		b[i] = byte(a<<4 | c)
	}
	return b, nil
}

// Delete removes an object when its supplied precondition holds.
func (s *S3) Delete(ctx context.Context, key string, o DeleteOptions) error {
	in := &s3.DeleteObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))}
	if o.IfMatch != "" {
		in.IfMatch = aws.String(o.IfMatch)
	}
	_, e := s.client.DeleteObject(ctx, in)
	return mapS3(e)
}

// List returns object metadata beneath prefix in key order.
func (s *S3) List(ctx context.Context, prefix string) ([]Metadata, error) {
	var out []Metadata
	var token *string
	for {
		r, e := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String(s.key(prefix)), ContinuationToken: token})
		if e != nil {
			return nil, mapS3(e)
		}
		for _, x := range r.Contents {
			k := aws.ToString(x.Key)
			base := s.loc.Prefix
			if base != "" {
				k = strings.TrimPrefix(k, base+"/")
			}
			out = append(out, Metadata{Key: k, ETag: aws.ToString(x.ETag), Size: aws.ToInt64(x.Size), LastModified: aws.ToTime(x.LastModified)})
		}
		if !aws.ToBool(r.IsTruncated) {
			break
		}
		token = r.NextContinuationToken
	}
	return out, nil
}
