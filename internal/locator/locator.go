package locator

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Locator identifies a repository root in an S3 bucket.
type Locator struct{ Bucket, Prefix string }

// Parse validates and parses an S3 remote locator.
func Parse(raw string) (Locator, error) {
	if strings.IndexByte(raw, 0) >= 0 {
		return Locator{}, fmt.Errorf("URL contains NUL")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return Locator{}, fmt.Errorf("URL contains control character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Locator{}, err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return Locator{}, fmt.Errorf("expected s3://<bucket>[/prefix]")
	}
	if u.Port() != "" || strings.Contains(u.Host, ":") {
		return Locator{}, fmt.Errorf("bucket must not include a port")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Locator{}, fmt.Errorf("userinfo, query, and fragment are forbidden")
	}
	if strings.Contains(u.Host, "\\") || strings.Contains(u.EscapedPath(), "\\") {
		return Locator{}, fmt.Errorf("backslashes are forbidden")
	}
	escaped := strings.TrimPrefix(u.EscapedPath(), "/")
	escaped = strings.TrimSuffix(escaped, "/")
	if escaped == "" {
		return Locator{Bucket: u.Host}, nil
	}
	parts := strings.Split(escaped, "/")
	decoded := make([]string, len(parts))
	for i, p := range parts {
		if p == "" {
			return Locator{}, fmt.Errorf("empty path segment")
		}
		d, err := url.PathUnescape(p)
		if err != nil {
			return Locator{}, fmt.Errorf("segment %d: %w", i, err)
		}
		if d == "" || d == "." || d == ".." || strings.ContainsAny(d, "/\\") || !utf8.ValidString(d) {
			return Locator{}, fmt.Errorf("invalid path segment %q", d)
		}
		for _, r := range d {
			if r < 0x20 || r == 0x7f {
				return Locator{}, fmt.Errorf("control character in path")
			}
		}
		decoded[i] = d
	}
	return Locator{Bucket: u.Host, Prefix: strings.Join(decoded, "/")}, nil
}

// ReservedPrefix returns the object-key prefix owned by git3.
func (l Locator) ReservedPrefix() string {
	if l.Prefix == "" {
		return ".git/git3/"
	}
	return l.Prefix + "/.git/git3/"
}

// Key resolves a managed relative key beneath the locator prefix.
func (l Locator) Key(relative string) string {
	if l.Prefix == "" {
		return relative
	}
	return l.Prefix + "/" + relative
}
func (l Locator) String() string {
	if l.Prefix == "" {
		return "s3://" + l.Bucket
	}
	return "s3://" + l.Bucket + "/" + l.Prefix
}

// ValidateManagedKey reports whether key belongs to the managed .git/git3 namespace.
func ValidateManagedKey(key string) error {
	if !strings.HasPrefix(key, ".git/git3/") {
		return fmt.Errorf("key outside reserved prefix")
	}
	for _, p := range strings.Split(key, "/") {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("invalid key segment")
		}
	}
	return nil
}
