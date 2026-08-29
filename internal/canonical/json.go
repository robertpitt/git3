package canonical

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
)

// MaxSafeInteger is the largest integer represented exactly by interoperable JSON numbers.
const MaxSafeInteger = uint64(9007199254740991)

// Marshal returns the RFC 8785 canonical JSON representation of v.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if !utf8.Valid(out) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	return jcs.Transform(out)
}

// UnmarshalStrict decodes one JSON value while rejecting duplicates and unknown fields.
func UnmarshalStrict(data []byte, v any, max int64) error {
	if int64(len(data)) > max {
		return fmt.Errorf("document exceeds %d bytes", max)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	if err := rejectDuplicatesAndNumbers(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := ensureEOF(d); err != nil {
		return err
	}
	return nil
}

// UnmarshalForward decodes one JSON value while permitting unknown fields.
func UnmarshalForward(data []byte, v any, max int64) error {
	if int64(len(data)) > max {
		return fmt.Errorf("document exceeds %d bytes", max)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	if err := rejectDuplicatesAndNumbers(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(v); err != nil {
		return err
	}
	return ensureEOF(d)
}

func ensureEOF(d *json.Decoder) error {
	var x any
	if err := d.Decode(&x); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicatesAndNumbers(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return scanValue(d)
}
func scanValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok {
					return fmt.Errorf("non-string key")
				}
				if seen[s] {
					return fmt.Errorf("duplicate key %q", s)
				}
				seen[s] = true
				if err := scanValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err := scanValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		default:
			return fmt.Errorf("unexpected delimiter")
		}
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") && !strings.HasPrefix(s, "-") {
			u, err := strconv.ParseUint(s, 10, 64)
			if err != nil || u > MaxSafeInteger {
				return fmt.Errorf("integer out of range %q", s)
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("invalid number %q", s)
		}
		canon, err := jcs.NumberToJSON(f)
		if err != nil || canon != s {
			return fmt.Errorf("non-canonical number %q", s)
		}
	}
	return nil
}
