package publish

import "testing"

func TestNormalizeKey(t *testing.T) {
	for in, want := range map[string]string{
		"sk-abc123":       "abc123",
		"abc123":          "abc123",
		"sk-abc123-7":     "abc123",
		"sk-abc123-7-xyz": "abc123",
	} {
		if got := normalizeKey(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestBearer(t *testing.T) {
	if k, ok := bearer("Bearer sk-x"); !ok || k != "sk-x" {
		t.Error("Bearer")
	}
	if k, ok := bearer("bearer   sk-x "); !ok || k != "sk-x" {
		t.Error("lowercase and spaces")
	}
	for _, h := range []string{"", "Basic abc", "Bearer ", "sk-x"} {
		if _, ok := bearer(h); ok {
			t.Errorf("%q accepted", h)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(1790000000, 42)
	a, b, ok := decodeCursor(c)
	if !ok || a != 1790000000 || b != 42 {
		t.Errorf("got %d %d %v", a, b, ok)
	}
	if _, _, ok := decodeCursor("not-a-cursor!"); ok {
		t.Error("garbage cursor accepted")
	}
}
