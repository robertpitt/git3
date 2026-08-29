package canonical

import "testing"

func TestRejectDuplicateAndNonInteger(t *testing.T) {
	for _, b := range []string{`{"a":1,"a":2}`, `{"a":1.0}`, `{"a":9007199254740992}`, `{"a":01}`} {
		var v map[string]any
		if UnmarshalForward([]byte(b), &v, 100) == nil {
			t.Errorf("accepted %s", b)
		}
	}
}
func TestMarshalNoHTMLRewrite(t *testing.T) {
	b, e := Marshal(map[string]string{"x": "<>&"})
	if e != nil {
		t.Fatal(e)
	}
	if string(b) != `{"x":"<>&"}` {
		t.Fatalf("%s", b)
	}
}
