package config

import "testing"

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "5KiB": 5 << 10, "2MiB": 2 << 20, "3GiB": 3 << 30, "1TiB": 1 << 40} {
		got, e := ParseBytes(in)
		if e != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, e)
		}
	}
	for _, x := range []string{"", "-1", "1KB", "1.5MiB"} {
		if _, e := ParseBytes(x); e == nil {
			t.Errorf("accepted %q", x)
		}
	}
}
func TestDefaultsValidate(t *testing.T) {
	if e := Defaults().Validate(); e != nil {
		t.Fatal(e)
	}
	c := Defaults()
	c.PartSize = 5 << 20
	if e := c.Validate(); e == nil {
		t.Fatal("accepted part size unable to address 1 TiB")
	}
	c = Defaults()
	c.Endpoint = "http://example.com"
	if e := c.Validate(); e == nil {
		t.Fatal("accepted insecure endpoint")
	}
}
