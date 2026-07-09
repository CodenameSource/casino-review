package market

import (
	"math"
	"strconv"
	"strings"
)

// FindingsBucket returns the outcome whose range contains count, for a
// findings-count market. Bucket labels are "N" (exactly N), "A-B" (inclusive),
// or "N+" (N or more). ok=false if no bucket matches (custom outcome sets that
// don't partition 0..∞) — the oracle then leaves the market for manual settling.
func FindingsBucket(count int, outcomes []string) (string, bool) {
	for _, o := range outcomes {
		if lo, hi, ok := parseBucket(o); ok && count >= lo && count <= hi {
			return o, true
		}
	}
	return "", false
}

func parseBucket(s string) (lo, hi int, ok bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return n, n, true
	}
	if rest, found := strings.CutSuffix(s, "+"); found {
		if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			return n, math.MaxInt, true
		}
		return 0, 0, false
	}
	if a, b, found := strings.Cut(s, "-"); found {
		lo, e1 := strconv.Atoi(strings.TrimSpace(a))
		hi, e2 := strconv.Atoi(strings.TrimSpace(b))
		if e1 == nil && e2 == nil && lo <= hi {
			return lo, hi, true
		}
	}
	return 0, 0, false
}
