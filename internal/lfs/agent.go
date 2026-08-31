// Package lfs implements the Git LFS custom-transfer protocol for git3.
package lfs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	"github.com/robertpitt/git3/internal/store"
)

const (
	defaultChunkSize   = int64(64 << 20)
	defaultConcurrency = 4
	maxObjectSize      = int64(1 << 40)
	maxProtocolLine    = 1 << 20
)

var oidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Backend contains the storage behavior needed by one transfer process.
type Backend struct {
	Store               ObjectStore
	DownloadChunkSize   int64
	DownloadConcurrency int
}

// ObjectStore is the narrow object-storage seam required by Git LFS transfers.
type ObjectStore interface {
	Head(context.Context, string) (store.Metadata, error)
	GetRange(context.Context, string, int64, int64) ([]byte, error)
	Put(context.Context, string, io.Reader, int64, store.PutOptions) (store.Metadata, error)
}

// OpenBackend resolves the remote named by an init message.
type OpenBackend func(context.Context, string, string) (Backend, error)

// Agent serves one Git LFS custom-transfer process.
type Agent struct {
	Open    OpenBackend
	In      io.Reader
	Out     io.Writer
	TempDir string
	writeMu sync.Mutex
	encoder *json.Encoder
}

type request struct {
	Event               string          `json:"event"`
	Operation           string          `json:"operation"`
	Remote              string          `json:"remote"`
	Concurrent          bool            `json:"concurrent"`
	ConcurrentTransfers int             `json:"concurrenttransfers"`
	OID                 string          `json:"oid"`
	Size                *int64          `json:"size"`
	Path                string          `json:"path"`
	Action              json.RawMessage `json:"action"`
}

type protocolError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	Event          string         `json:"event,omitempty"`
	OID            string         `json:"oid,omitempty"`
	Path           string         `json:"path,omitempty"`
	BytesSoFar     int64          `json:"bytesSoFar,omitempty"`
	BytesSinceLast int            `json:"bytesSinceLast,omitempty"`
	Error          *protocolError `json:"error,omitempty"`
}

// Run processes init, transfer, and terminate messages until the session ends.
func (a *Agent) Run(ctx context.Context) error {
	if a.Open == nil || a.In == nil || a.Out == nil {
		return fmt.Errorf("incomplete LFS agent configuration")
	}
	a.encoder = json.NewEncoder(a.Out)
	scanner := bufio.NewScanner(a.In)
	scanner.Buffer(make([]byte, 64<<10), maxProtocolLine)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	var init request
	if err := decodeRequest(scanner.Bytes(), &init); err != nil {
		return fmt.Errorf("decode LFS init: %w", err)
	}
	if init.Event != "init" || init.Operation != "upload" && init.Operation != "download" || init.Remote == "" {
		return fmt.Errorf("invalid LFS init message")
	}
	backend, err := a.Open(ctx, init.Operation, init.Remote)
	if err != nil {
		return a.write(response{Error: objectError(err)})
	}
	if backend.Store == nil {
		return a.write(response{Error: objectError(fmt.Errorf("remote has no object store"))})
	}
	if err = a.write(response{}); err != nil {
		return err
	}
	for scanner.Scan() {
		var req request
		if err = decodeRequest(scanner.Bytes(), &req); err != nil {
			return fmt.Errorf("decode LFS transfer: %w", err)
		}
		if req.Event == "terminate" {
			return nil
		}
		if req.Event != init.Operation {
			return fmt.Errorf("unexpected LFS event %q during %s", req.Event, init.Operation)
		}
		var path string
		if init.Operation == "upload" {
			err = a.upload(ctx, backend, req)
		} else {
			path, err = a.download(ctx, backend, req)
		}
		if err != nil {
			if writeErr := a.write(response{Event: "complete", OID: req.OID, Error: objectError(err)}); writeErr != nil {
				return writeErr
			}
			continue
		}
		if err = a.write(response{Event: "complete", OID: req.OID, Path: path}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func decodeRequest(line []byte, out *request) error {
	if len(line) == 0 {
		return fmt.Errorf("empty protocol line")
	}
	if err := json.Unmarshal(line, out); err != nil {
		return err
	}
	return nil
}

func objectError(err error) *protocolError {
	return &protocolError{Code: 2, Message: err.Error()}
}

func (a *Agent) write(v response) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.encoder.Encode(v)
}

func (a *Agent) progress(oid string, total int64, delta int) error {
	return a.write(response{Event: "progress", OID: oid, BytesSoFar: total, BytesSinceLast: delta})
}

func validateTransfer(req request, upload bool) (int64, error) {
	if !oidPattern.MatchString(req.OID) {
		return 0, fmt.Errorf("invalid LFS OID")
	}
	if req.Size == nil || *req.Size < 0 || *req.Size > maxObjectSize {
		return 0, fmt.Errorf("invalid LFS object size")
	}
	if upload && req.Path == "" {
		return 0, fmt.Errorf("upload path is required")
	}
	return *req.Size, nil
}

func objectKey(oid string) string {
	return ".git/git3/lfs/objects/" + oid[:2] + "/" + oid[2:4] + "/" + oid
}

func fileDigest(file *os.File) (int64, string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}

func (a *Agent) upload(ctx context.Context, backend Backend, req request) error {
	size, err := validateTransfer(req, true)
	if err != nil {
		return err
	}
	file, err := os.Open(req.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return fmt.Errorf("LFS upload size or file type mismatch")
	}
	n, digest, err := fileDigest(file)
	if err != nil {
		return err
	}
	if n != size || digest != req.OID {
		return fmt.Errorf("LFS upload integrity mismatch")
	}
	key := objectKey(req.OID)
	if metadata, headErr := backend.Store.Head(ctx, key); headErr == nil {
		if metadata.Size != size {
			return fmt.Errorf("remote LFS object size mismatch")
		}
		return verifyRemote(ctx, backend.Store, key, size, req.OID, chunkSize(backend))
	} else if !errors.Is(headErr, store.ErrNotFound) {
		return headErr
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = backend.Store.Put(ctx, key, file, size, store.PutOptions{IfNoneMatch: true, ContentSHA256: req.OID})
	if errors.Is(err, store.ErrPrecondition) {
		return verifyRemote(ctx, backend.Store, key, size, req.OID, chunkSize(backend))
	}
	return err
}

func verifyRemote(ctx context.Context, storage ObjectStore, key string, size int64, oid string, chunk int64) error {
	hash := sha256.New()
	for start := int64(0); start < size; {
		end := start + chunk - 1
		if end >= size {
			end = size - 1
		}
		part, err := storage.GetRange(ctx, key, start, end)
		if err != nil {
			return err
		}
		if int64(len(part)) != end-start+1 {
			return fmt.Errorf("short LFS range response")
		}
		_, _ = hash.Write(part)
		start = end + 1
	}
	if hex.EncodeToString(hash.Sum(nil)) != oid {
		return fmt.Errorf("remote LFS object integrity mismatch")
	}
	return nil
}

func (a *Agent) download(ctx context.Context, backend Backend, req request) (string, error) {
	size, err := validateTransfer(req, false)
	if err != nil {
		return "", err
	}
	key := objectKey(req.OID)
	metadata, err := backend.Store.Head(ctx, key)
	if err != nil {
		return "", err
	}
	if metadata.Size != size {
		return "", fmt.Errorf("remote LFS object size mismatch")
	}
	file, err := os.CreateTemp(a.TempDir, "git3-lfs-download-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err = file.Truncate(size); err != nil {
		return "", err
	}
	if err = a.downloadRanges(ctx, backend, req.OID, key, size, file); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	verify, err := os.Open(path)
	if err != nil {
		return "", err
	}
	n, digest, digestErr := fileDigest(verify)
	_ = verify.Close()
	if digestErr != nil {
		return "", digestErr
	}
	if n != size || digest != req.OID {
		return "", fmt.Errorf("downloaded LFS object integrity mismatch")
	}
	ok = true
	return path, nil
}

type byteRange struct{ start, end int64 }

func (a *Agent) downloadRanges(ctx context.Context, backend Backend, oid, key string, size int64, file *os.File) error {
	if size == 0 {
		return nil
	}
	workers := backend.DownloadConcurrency
	if workers <= 0 {
		workers = defaultConcurrency
	}
	if workers > 64 {
		workers = 64
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan byteRange)
	errs := make(chan error, 1)
	var completed int64
	var progressMu sync.Mutex
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for span := range jobs {
				part, err := backend.Store.GetRange(ctx, key, span.start, span.end)
				if err == nil && int64(len(part)) != span.end-span.start+1 {
					err = fmt.Errorf("short LFS range response")
				}
				if err == nil {
					_, err = file.WriteAt(part, span.start)
				}
				if err == nil {
					progressMu.Lock()
					completed += int64(len(part))
					err = a.progress(oid, completed, len(part))
					progressMu.Unlock()
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	chunk := chunkSize(backend)
	for start := int64(0); start < size; {
		end := start + chunk - 1
		if end >= size {
			end = size - 1
		}
		select {
		case jobs <- byteRange{start: start, end: end}:
			start = end + 1
		case <-ctx.Done():
			start = size
		}
	}
	close(jobs)
	group.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func chunkSize(backend Backend) int64 {
	if backend.DownloadChunkSize > 0 {
		return backend.DownloadChunkSize
	}
	return defaultChunkSize
}
