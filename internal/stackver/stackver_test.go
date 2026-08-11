package stackver

import "testing"

func TestParseAcceptsBothSpellings(t *testing.T) {
	// The whole reason this package exists: these two must compare equal.
	bare, err := Parse("1.9.0")
	if err != nil {
		t.Fatalf("bare spelling: %v", err)
	}
	tagged, err := Parse("v1.9.0")
	if err != nil {
		t.Fatalf("tag spelling: %v", err)
	}
	if bare.Compare(tagged) != 0 {
		t.Fatalf("%v != %v", bare, tagged)
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"", "1", "1.9", "1.9.0.1", "v", "1.9.x", "-1.0.0", "1.9.0-rc1", "latest",
	} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want error", in, got)
		}
	}
}

func TestCompareOrders(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.9.0", "1.9.0", 0},
		{"1.8.0", "1.9.0", -1},
		{"1.9.0", "1.8.0", 1},
		{"1.7.0", "1.10.0", -1}, // not string ordering
		{"2.0.0", "1.99.99", 1},
		{"1.1.0", "1.1.1", -1},
	}
	for _, c := range cases {
		a, err := Parse(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Compare(b); got != c.want {
			t.Errorf("Parse(%q).Compare(%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
