package locator

import "testing"

func TestParse(t *testing.T) {
	l, e := Parse("s3://Bucket/team%20one/repo/")
	if e != nil {
		t.Fatal(e)
	}
	if l.Bucket != "Bucket" || l.Prefix != "team one/repo" || l.ReservedPrefix() != "team one/repo/.git/git3/" {
		t.Fatalf("unexpected %#v", l)
	}
}
func TestParseRejectsEscapes(t *testing.T) {
	for _, s := range []string{"https://b/x", "s3://b/a//b", "s3://b/a/../b", "s3://u:p@b/x", "s3://b/x?q=1", "s3://b/a%2Fb"} {
		if _, e := Parse(s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
