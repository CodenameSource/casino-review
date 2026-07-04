package market

import (
	"fmt"
	"regexp"
	"strings"
)

var prShort = regexp.MustCompile(`^#(\d+)$`)
var prFull = regexp.MustCompile(`^pr:([^/#\s]+)/([^/#\s]+)#(\d+)$`)

// ext: keys are restricted to a tame charset: they get echoed into Slack
// messages (an unrestricted key like "ext:<!channel>" is a ping-injection) and
// they key the one-live-bounty unique index.
var extRef = regexp.MustCompile(`^ext:([A-Za-z0-9][A-Za-z0-9._/-]{0,63})$`)

// ParseContextRef normalizes user input into a canonical context ref.
// Accepted: "#123" (PR in the configured repo), "pr:owner/repo#123", "ext:KEY".
// pr: owner/repo are lowercased — GitHub treats them case-insensitively, and
// the one-live-bounty unique index keys on the canonical string, so case
// variants must not mint parallel bounties for the same PR.
func ParseContextRef(input, defaultOwner, defaultRepo string) (string, error) {
	input = strings.TrimSpace(input)
	if m := prShort.FindStringSubmatch(input); m != nil {
		return fmt.Sprintf("pr:%s/%s#%s", strings.ToLower(defaultOwner), strings.ToLower(defaultRepo), m[1]), nil
	}
	if m := prFull.FindStringSubmatch(input); m != nil {
		return fmt.Sprintf("pr:%s/%s#%s", strings.ToLower(m[1]), strings.ToLower(m[2]), m[3]), nil
	}
	if extRef.MatchString(input) {
		return input, nil
	}
	return "", fmt.Errorf("can't parse %q — use #123, pr:owner/repo#123, or ext:KEY (letters, digits, ._/-)", input)
}

// PRNumber extracts the PR number from a pr: ref (0, false for ext: refs).
func PRNumber(contextRef string) (int, bool) {
	m := prFull.FindStringSubmatch(contextRef)
	if m == nil {
		return 0, false
	}
	var n int
	fmt.Sscanf(m[3], "%d", &n)
	return n, true
}
