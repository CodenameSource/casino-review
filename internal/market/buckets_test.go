package market

import "testing"

func TestFindingsBucket(t *testing.T) {
	buckets := []string{"0", "1-2", "3-5", "6+"}
	cases := []struct {
		count int
		want  string
	}{
		{0, "0"}, {1, "1-2"}, {2, "1-2"}, {3, "3-5"}, {5, "3-5"}, {6, "6+"}, {99, "6+"},
	}
	for _, c := range cases {
		got, ok := FindingsBucket(c.count, buckets)
		if !ok || got != c.want {
			t.Errorf("FindingsBucket(%d) = %q,%v; want %q", c.count, got, ok, c.want)
		}
	}
	// A gappy custom set that doesn't cover the count returns ok=false.
	if _, ok := FindingsBucket(4, []string{"0", "1-2"}); ok {
		t.Error("count outside all buckets should not match")
	}
}
