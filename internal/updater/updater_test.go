package updater

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.8", "0.7", 1},
		{"v0.8.0", "0.7", 1},
		{"0.7.1", "0.7", 1},
		{"1.0", "0.99", 1},
		{"v0.7", "0.7.0", 0},
		{"0.6", "0.7", -1},
		{"0.7.0", "0.8", -1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
