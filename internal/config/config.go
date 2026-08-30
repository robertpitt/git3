package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Config contains the resolved client configuration.
type Config struct {
	Profile                  string
	Region                   string
	Endpoint                 string
	PathStyle                bool
	AllowInsecure            bool
	SSE                      string
	KMSKeyID                 string
	BucketKeyEnabled         *bool
	MultipartThreshold       int64
	PartSize                 int64
	UploadConcurrency        int
	DownloadChunkSize        int64
	DownloadConcurrency      int
	MaxAttempts              int
	CompactionFanout         int
	CompactAfterTransactions int
	CompactAfterBytes        int64
	LogFormat                string
}

// Defaults returns the documented default configuration.
func Defaults() Config {
	return Config{
		SSE:                      "inherit",
		MultipartThreshold:       100 << 20,
		PartSize:                 128 << 20,
		UploadConcurrency:        2,
		DownloadChunkSize:        64 << 20,
		DownloadConcurrency:      4,
		MaxAttempts:              5,
		LogFormat:                "human",
		CompactionFanout:         4,
		CompactAfterTransactions: 32,
		CompactAfterBytes:        128 << 20,
	}
}

// Load applies normal Git precedence, then named-remote overrides, then environment overrides.
func Load(remote string) (Config, error) {
	c := Defaults()
	defs := []struct{ env, global, suffix string }{
		{"AWS_PROFILE", "git3.profile", "git3Profile"},
		{"GIT3_REGION", "git3.region", "git3Region"},
		{"GIT3_ENDPOINT", "git3.endpoint", "git3Endpoint"},
		{"GIT3_PATH_STYLE", "git3.pathStyle", "git3PathStyle"},
		{"GIT3_ALLOW_INSECURE_ENDPOINT", "git3.allowInsecureEndpoint", "git3AllowInsecureEndpoint"},
		{"GIT3_SSE", "git3.sse", "git3Sse"},
		{"GIT3_KMS_KEY_ID", "git3.kmsKeyId", "git3KmsKeyId"},
		{"GIT3_BUCKET_KEY_ENABLED", "git3.bucketKeyEnabled", "git3BucketKeyEnabled"},
		{"GIT3_MULTIPART_THRESHOLD", "git3.multipartThreshold", "git3MultipartThreshold"},
		{"GIT3_PART_SIZE", "git3.partSize", "git3PartSize"},
		{"GIT3_UPLOAD_CONCURRENCY", "git3.uploadConcurrency", "git3UploadConcurrency"},
		{"GIT3_DOWNLOAD_CHUNK_SIZE", "git3.downloadChunkSize", "git3DownloadChunkSize"},
		{"GIT3_DOWNLOAD_CONCURRENCY", "git3.downloadConcurrency", "git3DownloadConcurrency"},
		{"GIT3_MAX_ATTEMPTS", "git3.maxAttempts", "git3MaxAttempts"},
		{"GIT3_LOG_FORMAT", "git3.logFormat", "git3LogFormat"},
		{"GIT3_COMPACTION_FANOUT", "git3.compactionFanout", "git3CompactionFanout"},
		{"GIT3_COMPACT_AFTER_TRANSACTIONS", "git3.compactAfterTransactions", "git3CompactAfterTransactions"},
		{"GIT3_COMPACT_AFTER_BYTES", "git3.compactAfterBytes", "git3CompactAfterBytes"},
	}
	values, e := readGitConfig()
	if e != nil {
		return c, e
	}
	for _, d := range defs {
		v, ok := values.global(d.global)
		if remote != "" {
			if remoteValue, found := values.remote(remote, d.suffix); found {
				v, ok = remoteValue, true
			}
		}
		if environmentValue, found := os.LookupEnv(d.env); found {
			v, ok = environmentValue, true
		}
		if ok {
			if e := apply(&c, d.env, v); e != nil {
				return c, e
			}
		}
	}
	return c, c.Validate()
}

type gitConfigEntry struct{ key, value string }
type gitConfigEntries []gitConfigEntry

func readGitConfig() (gitConfigEntries, error) {
	b, e := exec.Command("git", "config", "--null", "--get-regexp", `^(git3\.|remote\..*\.git3)`).Output()
	if e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("read Git configuration: %w", e)
	}
	var out gitConfigEntries
	for _, record := range bytes.Split(b, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(record, []byte{'\n'})
		if !ok {
			return nil, fmt.Errorf("invalid Git configuration record")
		}
		out = append(out, gitConfigEntry{key: string(key), value: string(value)})
	}
	return out, nil
}

func (v gitConfigEntries) global(key string) (string, bool) {
	for i := len(v) - 1; i >= 0; i-- {
		if strings.EqualFold(v[i].key, key) {
			return v[i].value, true
		}
	}
	return "", false
}

func (v gitConfigEntries) remote(remote, suffix string) (string, bool) {
	for i := len(v) - 1; i >= 0; i-- {
		key := v[i].key
		if len(key) < len("remote.") || !strings.EqualFold(key[:len("remote.")], "remote.") {
			continue
		}
		rest := key[len("remote."):]
		marker := "." + suffix
		if len(rest) > len(marker) && rest[:len(rest)-len(marker)] == remote && strings.EqualFold(rest[len(rest)-len(marker):], marker) {
			return v[i].value, true
		}
	}
	return "", false
}
func apply(c *Config, k, v string) error {
	var e error
	var n int64
	var i int
	var b bool
	switch k {
	case "AWS_PROFILE":
		c.Profile = v
	case "GIT3_REGION":
		c.Region = v
	case "GIT3_ENDPOINT":
		c.Endpoint = v
	case "GIT3_SSE":
		c.SSE = v
	case "GIT3_KMS_KEY_ID":
		c.KMSKeyID = v
	case "GIT3_LOG_FORMAT":
		c.LogFormat = v
	case "GIT3_PATH_STYLE", "GIT3_ALLOW_INSECURE_ENDPOINT", "GIT3_BUCKET_KEY_ENABLED":
		b, e = parseBool(v)
		if k == "GIT3_PATH_STYLE" {
			c.PathStyle = b
		} else if k == "GIT3_ALLOW_INSECURE_ENDPOINT" {
			c.AllowInsecure = b
		} else {
			c.BucketKeyEnabled = &b
		}
	case "GIT3_MULTIPART_THRESHOLD", "GIT3_PART_SIZE", "GIT3_DOWNLOAD_CHUNK_SIZE", "GIT3_COMPACT_AFTER_BYTES":
		n, e = ParseBytes(v)
		if k == "GIT3_MULTIPART_THRESHOLD" {
			c.MultipartThreshold = n
		} else if k == "GIT3_PART_SIZE" {
			c.PartSize = n
		} else if k == "GIT3_DOWNLOAD_CHUNK_SIZE" {
			c.DownloadChunkSize = n
		} else {
			c.CompactAfterBytes = n
		}
	default:
		i, e = strconv.Atoi(v)
		if e == nil && (i < 1 || i > 1024) {
			e = fmt.Errorf("value out of range")
		}
		if k == "GIT3_UPLOAD_CONCURRENCY" {
			c.UploadConcurrency = i
		} else if k == "GIT3_DOWNLOAD_CONCURRENCY" {
			c.DownloadConcurrency = i
		} else if k == "GIT3_MAX_ATTEMPTS" {
			c.MaxAttempts = i
		} else if k == "GIT3_COMPACTION_FANOUT" {
			c.CompactionFanout = i
		} else {
			c.CompactAfterTransactions = i
		}
	}
	if e != nil {
		return fmt.Errorf("%s: %w", k, e)
	}
	return nil
}

// Validate reports whether all configuration values are mutually consistent and supported.
func (c Config) Validate() error {
	if c.PartSize < 5<<20 || c.PartSize < (1<<40+9999)/10000 {
		return fmt.Errorf("part size cannot support 1 TiB within 10,000 parts")
	}
	if c.PartSize > 5<<30 || c.MultipartThreshold < 0 || c.MultipartThreshold > 1<<40 || c.DownloadChunkSize < 1<<20 || c.DownloadChunkSize > 5<<30 || c.CompactAfterBytes < 1 || c.CompactAfterBytes > 1<<40 {
		return fmt.Errorf("byte setting outside supported bounds")
	}
	if c.UploadConcurrency < 1 || c.UploadConcurrency > 16 || c.DownloadConcurrency < 1 || c.DownloadConcurrency > 64 || c.MaxAttempts < 1 || c.MaxAttempts > 20 || c.CompactionFanout < 2 || c.CompactionFanout > 1024 || c.CompactAfterTransactions < 1 || c.CompactAfterTransactions > 100000 {
		return fmt.Errorf("integer setting outside supported bounds")
	}
	if c.Endpoint != "" {
		u, e := url.Parse(c.Endpoint)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && (u.Scheme != "http" || !c.AllowInsecure) {
			return fmt.Errorf("invalid or insecure endpoint")
		}
	}
	if c.SSE != "inherit" && c.SSE != "s3" && c.SSE != "kms" {
		return fmt.Errorf("invalid SSE mode")
	}
	if c.SSE != "kms" && (c.KMSKeyID != "" || c.BucketKeyEnabled != nil) {
		return fmt.Errorf("KMS settings require sse=kms")
	}
	if c.LogFormat != "human" && c.LogFormat != "json" {
		return fmt.Errorf("invalid log format")
	}
	return nil
}
func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", s)
	}
}

// ParseBytes parses a non-negative byte count with an optional binary unit suffix.
func ParseBytes(s string) (int64, error) {
	for _, x := range []struct {
		u string
		m uint64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"", 1}} {
		if strings.HasSuffix(s, x.u) {
			n := strings.TrimSuffix(s, x.u)
			if n == "" {
				continue
			}
			v, e := strconv.ParseUint(n, 10, 63)
			if e != nil || v > uint64(^uint64(0)>>1)/x.m {
				return 0, fmt.Errorf("invalid byte quantity %q", s)
			}
			return int64(v * x.m), nil
		}
	}
	return 0, fmt.Errorf("invalid byte suffix")
}
