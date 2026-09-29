package semver

import "testing"

func TestParseRejectionCodes(t *testing.T) {
	cases := []struct {
		in   string
		code ErrorCode
	}{
		{"", ErrCodeEmpty},
		{"1.2.3", ErrCodeMissingVPrefix},
		{"v1.2", ErrCodeBadNumber},
		{"v1.x.3", ErrCodeBadNumber},
		{"v1.2.3-", ErrCodeBadSuffix},
		{"v1.2.3+meta", ErrCodeBadSuffix},
		{"v01.2.3", ErrCodeBadNumber},
	}
	for _, c := range cases {
		v, err := Parse(c.in)
		if v != nil || err == nil {
			t.Fatalf("Parse(%q) expected error code %v", c.in, c.code)
		}
		ve, ok := AsVersionError(err)
		if !ok || ve.Code != c.code {
			t.Fatalf("Parse(%q) got %v, want code %v", c.in, err, c.code)
		}
		t.Logf("input=%q rejected as %v (%s)", c.in, ve.Code, ve)
	}
}

func TestParseOK(t *testing.T) {
	for _, in := range []string{"v1.2.3", "v0.0.0", "v2.0.1-rc.1", "v1.0.0+incompatible"} {
		v, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if v.String() != in {
			t.Fatalf("round trip got %q want %q", v.String(), in)
		}
	}
}

func TestCompare(t *testing.T) {
	order := []string{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-beta", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.1.0", "v2.0.0"}
	for i := 1; i < len(order); i++ {
		a, _ := Parse(order[i-1])
		b, _ := Parse(order[i])
		if Compare(a, b) != -1 {
			t.Fatalf("expected %s < %s", order[i-1], order[i])
		}
	}
	inc, _ := Parse("v1.0.0+incompatible")
	rel, _ := Parse("v1.0.0")
	if Compare(inc, rel) != 0 {
		t.Fatal("+incompatible must not affect precedence")
	}
}
