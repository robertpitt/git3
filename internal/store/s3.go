package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/robertpitt/git3/internal/config"
	"github.com/robertpitt/git3/internal/errs"
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
			return errs.E(errs.AuthFailed, "s3", err)
		case "NoSuchBucket":
			return errs.E(errs.BucketNotFound, "s3", err)
		case "RequestTimeout", "SlowDown", "ServiceUnavailable", "InternalError":
			return errs.E(errs.NetworkExhausted, "s3", err)
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
			return errs.E(errs.AuthFailed, "s3", err)
		case 408, 429, 500, 502, 503, 504:
			return errs.E(errs.NetworkExhausted, "s3", err)
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

// OpenRange opens the inclusive byte range [start, end].
func (s *S3) OpenRange(ctx context.Context, key string, start, end int64) (Range, error) {
	if start < 0 || end < start {
		return Range{}, fmt.Errorf("invalid range")
	}
	out, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), Range: aws.String(fmt.Sprintf("bytes=%d-%d", start, end))})
	if e != nil {
		return Range{}, mapS3(e)
	}
	want := end - start + 1
	a, b, total, e := parseContentRange(aws.ToString(out.ContentRange))
	if e != nil || a != start || b != end || total <= end || out.ContentLength == nil || *out.ContentLength != want {
		_ = out.Body.Close()
		return Range{}, fmt.Errorf("invalid range response metadata")
	}
	return Range{Body: out.Body, Size: want, TotalSize: total}, nil
}

func parseContentRange(v string) (int64, int64, int64, error) {
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, fmt.Errorf("invalid content range")
	}
	span, totalText, ok := strings.Cut(strings.TrimPrefix(v, "bytes "), "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("invalid content range")
	}
	startText, endText, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("invalid content range")
	}
	start, e1 := strconv.ParseInt(startText, 10, 64)
	end, e2 := strconv.ParseInt(endText, 10, 64)
	total, e3 := strconv.ParseInt(totalText, 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("invalid content range")
	}
	return start, end, total, nil
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
	if size > 0 && size >= s.cfg.MultipartThreshold && o.IfMatch == "" {
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
	workers := s.cfg.UploadConcurrency
	if workers < 1 {
		workers = 2
	}
	type uploadResult struct {
		part   types.CompletedPart
		buffer []byte
		size   int64
		err    error
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan uploadResult, workers)
	free := make([][]byte, workers)
	var parts []types.CompletedPart
	var total int64
	var inflight int
	accept := func(result uploadResult) error {
		inflight--
		free = append(free, result.buffer[:cap(result.buffer)])
		if result.err != nil {
			return result.err
		}
		parts = append(parts, result.part)
		total += result.size
		return nil
	}
	drain := func() {
		for inflight > 0 {
			_ = accept(<-results)
		}
	}
	nPart := int32(1)
	doneReading := false
	for !doneReading {
		if len(free) == 0 {
			if e = accept(<-results); e != nil {
				cancel()
				drain()
				return Metadata{}, e
			}
		}
		last := len(free) - 1
		buf := free[last]
		free = free[:last]
		if buf == nil {
			buf = make([]byte, int(s.cfg.PartSize))
		}
		n, er := io.ReadFull(r, buf)
		if er == io.EOF && n == 0 {
			free = append(free, buf)
			break
		}
		if er != nil && er != io.ErrUnexpectedEOF {
			cancel()
			drain()
			return Metadata{}, er
		}
		if nPart > 10000 {
			cancel()
			drain()
			return Metadata{}, fmt.Errorf("multipart upload exceeds 10,000 parts")
		}
		partNumber := nPart
		inflight++
		go func(buffer []byte, size int, partNumber int32) {
			up, uploadErr := s.client.UploadPart(uploadCtx, &s3.UploadPartInput{Bucket: &s.bucket, Key: aws.String(s.key(key)), UploadId: started.UploadId, PartNumber: aws.Int32(partNumber), Body: bytes.NewReader(buffer[:size]), ContentLength: aws.Int64(int64(size)), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
			result := uploadResult{buffer: buffer, size: int64(size), err: mapS3(uploadErr)}
			if uploadErr == nil {
				result.part = types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(partNumber), ChecksumSHA256: up.ChecksumSHA256}
			}
			results <- result
		}(buf, n, partNumber)
		nPart++
		if er == io.ErrUnexpectedEOF {
			doneReading = true
		}
	ready:
		for inflight > 0 {
			select {
			case result := <-results:
				if e = accept(result); e != nil {
					cancel()
					drain()
					return Metadata{}, e
				}
			default:
				break ready
			}
		}
	}
	for inflight > 0 {
		if e = accept(<-results); e != nil {
			cancel()
			drain()
			return Metadata{}, e
		}
	}
	sort.Slice(parts, func(i, j int) bool { return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber) })
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

// Walk visits object metadata beneath prefix in key order, one S3 page at a time.
func (s *S3) Walk(ctx context.Context, prefix string, visit func(Metadata) error) error {
	var token *string
	for {
		r, e := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String(s.key(prefix)), ContinuationToken: token})
		if e != nil {
			return mapS3(e)
		}
		for _, x := range r.Contents {
			k := aws.ToString(x.Key)
			base := s.loc.Prefix
			if base != "" {
				k = strings.TrimPrefix(k, base+"/")
			}
			if e = visit(Metadata{Key: k, ETag: aws.ToString(x.ETag), Size: aws.ToInt64(x.Size), LastModified: aws.ToTime(x.LastModified)}); e != nil {
				return e
			}
		}
		if !aws.ToBool(r.IsTruncated) {
			break
		}
		if r.NextContinuationToken == nil || aws.ToString(r.NextContinuationToken) == "" {
			return fmt.Errorf("truncated object listing omitted continuation token")
		}
		if token != nil && aws.ToString(r.NextContinuationToken) == aws.ToString(token) {
			return fmt.Errorf("truncated object listing repeated continuation token")
		}
		token = r.NextContinuationToken
	}
	return nil
}
