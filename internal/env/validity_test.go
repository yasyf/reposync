package env

import "testing"

func TestValidKey(t *testing.T) {
	cases := []struct {
		key string
		ok  bool
	}{
		{"A", true},
		{"API_KEY", true},
		{"_underscore", true},
		{"K9", true},
		{"9K", false},
		{"", false},
		{"A B", false},
		{"A-B", false},
		{"FOO\nBAR", false},
		{"a.b", false},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			if got := ValidKey(c.key); got != c.ok {
				t.Errorf("ValidKey(%q) = %v, want %v", c.key, got, c.ok)
			}
		})
	}
}

func TestValidValue(t *testing.T) {
	cases := []struct {
		val string
		ok  bool
	}{
		{"secret", true},
		{"", true},
		{`"quoted spaces"`, true},
		{"with\ttab", true},
		{"trailing-cr\r", true},
		{"line1\nline2", false},
		{"\n", false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			if got := ValidValue(c.val); got != c.ok {
				t.Errorf("ValidValue(%q) = %v, want %v", c.val, got, c.ok)
			}
		})
	}
}
