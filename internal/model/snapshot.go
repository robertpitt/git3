package model

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// MarshalText returns the canonical line-oriented snapshot representation.
func (s Snapshot) MarshalText() ([]byte, error) {
	if !validUUID(s.RepositoryID) || (s.ObjectFormat != "sha1" && s.ObjectFormat != "sha256") || len(s.Refs) > MaxRefs {
		return nil, fmt.Errorf("invalid snapshot header")
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, "git3-ref-snapshot 1")
	fmt.Fprintln(&b, "repository "+s.RepositoryID)
	fmt.Fprintln(&b, "object-format "+s.ObjectFormat)
	fmt.Fprintf(&b, "generation %d\n", s.Generation)
	tx := "-"
	if s.TransactionID != nil {
		if !validUUID(*s.TransactionID) {
			return nil, fmt.Errorf("invalid transaction UUID")
		}
		tx = *s.TransactionID
	}
	fmt.Fprintln(&b, "transaction "+tx)
	fmt.Fprintln(&b)
	for _, r := range SortedRefs(s.Refs) {
		oid := s.Refs[r]
		if !validRefBasic(r) || !ValidOID(oid, s.ObjectFormat) {
			return nil, fmt.Errorf("invalid ref record")
		}
		fmt.Fprintf(&b, "%s %s\n", oid, r)
	}
	return b.Bytes(), nil
}

// ParseSnapshot parses and validates a canonical line-oriented snapshot.
func ParseSnapshot(data []byte) (Snapshot, error) {
	if len(data) > MaxSnapshot {
		return Snapshot{}, fmt.Errorf("snapshot too large")
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return Snapshot{}, fmt.Errorf("snapshot must end with LF")
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), MaxSnapshot)
	lines := []string{}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return Snapshot{}, err
	}
	if len(lines) < 6 || lines[0] != "git3-ref-snapshot 1" || lines[5] != "" {
		return Snapshot{}, fmt.Errorf("invalid snapshot grammar")
	}
	pref := func(i int, p string) (string, error) {
		if !strings.HasPrefix(lines[i], p) {
			return "", fmt.Errorf("invalid snapshot header")
		}
		return strings.TrimPrefix(lines[i], p), nil
	}
	repo, e := pref(1, "repository ")
	if e != nil {
		return Snapshot{}, e
	}
	format, e := pref(2, "object-format ")
	if e != nil {
		return Snapshot{}, e
	}
	gs, e := pref(3, "generation ")
	if e != nil {
		return Snapshot{}, e
	}
	gen, e := strconv.ParseUint(gs, 10, 53)
	if e != nil {
		return Snapshot{}, e
	}
	ts, e := pref(4, "transaction ")
	if e != nil {
		return Snapshot{}, e
	}
	var tid *string
	if ts != "-" {
		tid = &ts
	}
	s := Snapshot{RepositoryID: repo, ObjectFormat: format, Generation: gen, TransactionID: tid, Refs: map[string]string{}}
	if !validUUID(repo) || (format != "sha1" && format != "sha256") || (gen == 0 && tid != nil) || (gen > 0 && (tid == nil || !validUUID(*tid))) {
		return Snapshot{}, fmt.Errorf("invalid snapshot header values")
	}
	last := ""
	for _, line := range lines[6:] {
		i := strings.IndexByte(line, ' ')
		if i < 1 {
			return Snapshot{}, fmt.Errorf("invalid ref record")
		}
		oid, r := line[:i], line[i+1:]
		if r <= last || !validRefBasic(r) || !ValidOID(oid, format) {
			return Snapshot{}, fmt.Errorf("invalid or unsorted ref")
		}
		last = r
		s.Refs[r] = oid
	}
	if len(s.Refs) > MaxRefs {
		return Snapshot{}, fmt.Errorf("too many refs")
	}
	if s.Generation == 0 && len(s.Refs) != 0 {
		return Snapshot{}, fmt.Errorf("generation-zero snapshot contains refs")
	}
	return s, nil
}
