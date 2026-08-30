package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/robertpitt/git3/internal/config"
	"github.com/robertpitt/git3/internal/locator"
)

type mockS3API struct {
	getObject               func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	headObject              func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	putObject               func(*s3.PutObjectInput) (*s3.PutObjectOutput, error)
	createMultipartUpload   func(*s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error)
	uploadPart              func(*s3.UploadPartInput) (*s3.UploadPartOutput, error)
	completeMultipartUpload func(*s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error)
	abortMultipartUpload    func(*s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error)
	deleteObject            func(*s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	listObjectsV2           func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
}

func unexpected(name string) error { return errors.New("unexpected S3 call: " + name) }

func (m *mockS3API) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if m.getObject == nil {
		return nil, unexpected("GetObject")
	}
	return m.getObject(in)
}
func (m *mockS3API) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headObject == nil {
		return nil, unexpected("HeadObject")
	}
	return m.headObject(in)
}
func (m *mockS3API) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if m.putObject == nil {
		return nil, unexpected("PutObject")
	}
	return m.putObject(in)
}
func (m *mockS3API) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	if m.createMultipartUpload == nil {
		return nil, unexpected("CreateMultipartUpload")
	}
	return m.createMultipartUpload(in)
}
func (m *mockS3API) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if m.uploadPart == nil {
		return nil, unexpected("UploadPart")
	}
	return m.uploadPart(in)
}
func (m *mockS3API) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if m.completeMultipartUpload == nil {
		return nil, unexpected("CompleteMultipartUpload")
	}
	return m.completeMultipartUpload(in)
}
func (m *mockS3API) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	if m.abortMultipartUpload == nil {
		return nil, unexpected("AbortMultipartUpload")
	}
	return m.abortMultipartUpload(in)
}
func (m *mockS3API) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if m.deleteObject == nil {
		return nil, unexpected("DeleteObject")
	}
	return m.deleteObject(in)
}
func (m *mockS3API) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.listObjectsV2 == nil {
		return nil, unexpected("ListObjectsV2")
	}
	return m.listObjectsV2(in)
}

func testS3(client s3API) *S3 {
	cfg := config.Defaults()
	return &S3{client: client, bucket: "bucket", loc: locator.Locator{Bucket: "bucket", Prefix: "repo"}, cfg: cfg}
}

func TestLoadAWSConfigUsesExplicitProfile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	credentialsPath := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configPath, []byte("[profile account-role]\nregion = eu-west-2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, []byte("[account-role]\naws_access_key_id = test\naws_secret_access_key = test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsPath)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	cfg := config.Defaults()
	cfg.Profile = "account-role"
	loaded, err := loadAWSConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Region != "eu-west-2" {
		t.Fatalf("region = %q", loaded.Region)
	}
	cfg.SSE = "kms"
	store, err := NewS3(context.Background(), locator.Locator{Bucket: "bucket"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if store.bucket != "bucket" || store.encryption != types.ServerSideEncryptionAwsKms {
		t.Fatalf("store configuration = %#v", store)
	}
}

func TestS3GetAndRangeBuildRequests(t *testing.T) {
	modified := time.Unix(123, 0).UTC()
	mock := &mockS3API{}
	mock.getObject = func(in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		if aws.ToString(in.Key) != "repo/.git/git3/HEAD" || aws.ToString(in.IfNoneMatch) != "etag-old" {
			t.Fatalf("unexpected get input: %#v", in)
		}
		return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("head")), ContentLength: aws.Int64(4), ETag: aws.String("etag-new"), LastModified: &modified}, nil
	}
	store := testS3(mock)
	object, err := store.Get(context.Background(), ".git/git3/HEAD", "etag-old")
	if err != nil {
		t.Fatal(err)
	}
	if string(object.Body) != "head" || object.ETag != "etag-new" || object.Size != 4 || !object.LastModified.Equal(modified) {
		t.Fatalf("object = %#v", object)
	}

	mock.getObject = func(in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		if aws.ToString(in.Range) != "bytes=2-4" {
			t.Fatalf("range = %q", aws.ToString(in.Range))
		}
		return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("234"))}, nil
	}
	part, err := store.GetRange(context.Background(), ".git/git3/wal/id.pack", 2, 4)
	if err != nil || string(part) != "234" {
		t.Fatalf("range = %q, %v", part, err)
	}
}

func TestS3GetRejectsOversizedMetadata(t *testing.T) {
	mock := &mockS3API{getObject: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("")), ContentLength: aws.Int64((2 << 20) + 1)}, nil
	}}
	_, err := testS3(mock).Get(context.Background(), ".git/git3/HEAD", "")
	if err == nil || !strings.Contains(err.Error(), "exceeds read limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestS3PutSingleSetsConditionsChecksumAndEncryption(t *testing.T) {
	body := []byte("payload")
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	mock := &mockS3API{putObject: func(in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		got, err := io.ReadAll(in.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) || aws.ToString(in.IfNoneMatch) != "*" || aws.ToString(in.ChecksumSHA256) != base64.StdEncoding.EncodeToString(digest[:]) {
			t.Fatalf("unexpected put input: %#v body=%q", in, got)
		}
		if in.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(in.SSEKMSKeyId) != "alias/repository" || !aws.ToBool(in.BucketKeyEnabled) {
			t.Fatalf("missing encryption settings: %#v", in)
		}
		return &s3.PutObjectOutput{ETag: aws.String("etag")}, nil
	}}
	store := testS3(mock)
	store.encryption = types.ServerSideEncryptionAwsKms
	store.cfg.KMSKeyID = "alias/repository"
	enabled := true
	store.cfg.BucketKeyEnabled = &enabled
	metadata, err := store.Put(context.Background(), ".git/git3/objects/value", bytes.NewReader(body), int64(len(body)), PutOptions{IfNoneMatch: true, ContentSHA256: digestHex})
	if err != nil || metadata.ETag != "etag" || metadata.Size != int64(len(body)) {
		t.Fatalf("metadata = %#v, %v", metadata, err)
	}
}

func TestS3MultipartCompletesAndAbortsOnFailure(t *testing.T) {
	var uploaded [][]byte
	completed := false
	aborted := false
	mock := &mockS3API{}
	mock.createMultipartUpload = func(in *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error) {
		if aws.ToString(in.Key) != "repo/.git/git3/wal/id.pack" {
			t.Fatalf("key = %q", aws.ToString(in.Key))
		}
		return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload")}, nil
	}
	mock.uploadPart = func(in *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
		body, err := io.ReadAll(in.Body)
		if err != nil {
			t.Fatal(err)
		}
		uploaded = append(uploaded, body)
		part := aws.ToInt32(in.PartNumber)
		return &s3.UploadPartOutput{ETag: aws.String("part"), ChecksumSHA256: aws.String("sum" + string(rune('0'+part)))}, nil
	}
	mock.completeMultipartUpload = func(in *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error) {
		completed = true
		if aws.ToString(in.IfNoneMatch) != "*" || len(in.MultipartUpload.Parts) != 3 {
			t.Fatalf("complete input = %#v", in)
		}
		return &s3.CompleteMultipartUploadOutput{ETag: aws.String("multipart-etag")}, nil
	}
	mock.abortMultipartUpload = func(*s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error) {
		aborted = true
		return &s3.AbortMultipartUploadOutput{}, nil
	}
	store := testS3(mock)
	store.cfg.MultipartThreshold = 4
	store.cfg.PartSize = 5
	metadata, err := store.Put(context.Background(), ".git/git3/wal/id.pack", strings.NewReader("abcdefghijkl"), 12, PutOptions{IfNoneMatch: true})
	if err != nil || metadata.Size != 12 || !completed || aborted || len(uploaded) != 3 {
		t.Fatalf("metadata=%#v uploaded=%q completed=%t aborted=%t err=%v", metadata, uploaded, completed, aborted, err)
	}

	mock.uploadPart = func(*s3.UploadPartInput) (*s3.UploadPartOutput, error) { return nil, errors.New("upload failed") }
	completed, aborted = false, false
	_, err = store.Put(context.Background(), ".git/git3/wal/id.pack", strings.NewReader("abcdef"), 6, PutOptions{})
	if err == nil || completed || !aborted {
		t.Fatalf("completed=%t aborted=%t err=%v", completed, aborted, err)
	}
}

func TestS3HeadDeleteListAndErrorMapping(t *testing.T) {
	modified := time.Unix(456, 0).UTC()
	page := 0
	mock := &mockS3API{}
	mock.headObject = func(in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		return &s3.HeadObjectOutput{ContentLength: aws.Int64(9), ETag: aws.String("head-etag"), LastModified: &modified}, nil
	}
	mock.deleteObject = func(in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
		if aws.ToString(in.IfMatch) != "delete-etag" {
			t.Fatalf("if-match = %q", aws.ToString(in.IfMatch))
		}
		return &s3.DeleteObjectOutput{}, nil
	}
	mock.listObjectsV2 = func(in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		page++
		if aws.ToString(in.Prefix) != "repo/.git/git3/" {
			t.Fatalf("prefix = %q", aws.ToString(in.Prefix))
		}
		key := "repo/.git/git3/a"
		if page == 2 {
			key = "repo/.git/git3/b"
		}
		return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: &key, Size: aws.Int64(int64(page)), ETag: aws.String("etag"), LastModified: &modified}}, IsTruncated: aws.Bool(page == 1), NextContinuationToken: aws.String("next")}, nil
	}
	store := testS3(mock)
	metadata, err := store.Head(context.Background(), ".git/git3/value")
	if err != nil || metadata.Size != 9 || metadata.ETag != "head-etag" {
		t.Fatalf("head = %#v, %v", metadata, err)
	}
	if err = store.Delete(context.Background(), ".git/git3/value", DeleteOptions{IfMatch: "delete-etag"}); err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), ".git/git3/")
	if err != nil || len(objects) != 2 || objects[0].Key != ".git/git3/a" || objects[1].Key != ".git/git3/b" {
		t.Fatalf("list = %#v, %v", objects, err)
	}

	for code, want := range map[string]error{"NoSuchKey": ErrNotFound, "NotModified": ErrNotModified, "PreconditionFailed": ErrPrecondition} {
		err = mapS3(&smithy.GenericAPIError{Code: code, Message: "test"})
		if !errors.Is(err, want) {
			t.Errorf("mapS3(%s) = %v", code, err)
		}
	}
}
