package market

import (
	"strings"
	"testing"
)

func TestParseContextRef(t *testing.T) {
	good := map[string]string{
		"#123":                 "pr:mandel-ai/mandel#123",
		"pr:other/repo#9":      "pr:other/repo#9",
		"ext:PROJ-42":          "ext:PROJ-42",
		" #7 ":                 "pr:mandel-ai/mandel#7",
		"ext:tracker/TASK-001": "ext:tracker/TASK-001",
	}
	for in, want := range good {
		got, err := ParseContextRef(in, "mandel-ai", "mandel")
		if err != nil || got != want {
			t.Errorf("ParseContextRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "123", "pr:#123", "pr:owner#123", "https://github.com/o/r/pull/1",
		// ext: charset is restricted — these get echoed into Slack messages,
		// so markup like <!channel> must never survive parsing.
		"ext:<!channel>", "ext:<@U123>", "ext:<https://evil.com|x>", "ext:" + strings.Repeat("k", 65)} {
		if _, err := ParseContextRef(bad, "o", "r"); err == nil {
			t.Errorf("ParseContextRef(%q): expected error", bad)
		}
	}

	// Case folding: GitHub owner/repo are case-insensitive; case variants must
	// canonicalize identically or the one-live-bounty index mints duplicates.
	a, _ := ParseContextRef("pr:Foo/Bar#5", "o", "r")
	b, _ := ParseContextRef("pr:foo/bar#5", "o", "r")
	if a != b || a != "pr:foo/bar#5" {
		t.Fatalf("case variants must canonicalize: %q vs %q", a, b)
	}
	c, _ := ParseContextRef("#5", "MiXeD", "CaSe")
	if c != "pr:mixed/case#5" {
		t.Fatalf("default owner/repo not folded: %q", c)
	}
}

func TestPRNumber(t *testing.T) {
	if n, ok := PRNumber("pr:o/r#123"); !ok || n != 123 {
		t.Fatalf("PRNumber = %d %v", n, ok)
	}
	if _, ok := PRNumber("ext:KEY"); ok {
		t.Fatal("ext ref should not have a PR number")
	}
}

func TestKinds(t *testing.T) {
	if got := Kinds["bounty"].Outcomes(nil); len(got) != 1 || got[0] != "merged" {
		t.Fatalf("bounty outcomes = %v", got)
	}
	if got := Kinds["merge-by"].Outcomes(nil); len(got) != 2 {
		t.Fatalf("merge-by outcomes = %v", got)
	}
	if err := Kinds["merge-by"].ValidateSpec(map[string]any{}); err == nil {
		t.Fatal("merge-by without deadline should error")
	}
	if err := Kinds["merge-by"].ValidateSpec(map[string]any{"deadline": "2026-07-10T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	got := Kinds["findings-count"].Outcomes(map[string]any{"buckets": []any{"0", "1+", "broken", 42}})
	if len(got) != 3 { // 42 dropped
		t.Fatalf("custom buckets = %v", got)
	}
}

func TestBucketFor(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1-2", 2: "1-2", 3: "3-5", 5: "3-5", 6: "6+", 40: "6+"}
	for in, want := range cases {
		if got := BucketFor(in); got != want {
			t.Errorf("BucketFor(%d) = %q, want %q", in, got, want)
		}
	}
}
