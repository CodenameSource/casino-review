// Package spin is the one place the slot-machine GIF choreography lives:
// render the reel landing on the chosen entry, host it on the assets branch,
// and post the GIF comment. Reused by review spins today and judge spins (P4).
package spin

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"casino-review/internal/github"
	"casino-review/internal/slots"
	"casino-review/internal/templates"
)

// AssetDir is the folder on the assets branch GIFs live in; the TTL cleanup
// prunes by the timestamp prefix of names inside it.
const AssetDir = "casino"

// RandSeed returns a non-reproducible seed from the OS CSPRNG (the comment ID
// is public and monotonic — seeding from it would let anyone replay the GIF
// and read the winner early), falling back to the wall clock.
func RandSeed() int64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// Spinner posts slot spins on PRs.
type Spinner struct {
	GH     *github.Client // monitored repo (comments)
	Assets *github.Client // GIF host (public repo for permanent raw URLs)
	Branch string         // orphan assets branch
}

// Spin renders a reel over entries landing on chosen, hosts the GIF, and posts
// it as a comment on the PR. uniq disambiguates the asset filename (job ID).
// bonusLabel != "" appends the bonus round (the addon roll hit) — the roll
// happens before rendering so the animation always matches what actually runs.
func (s *Spinner) Spin(prNumber int, entries []string, chosen int, uniq int64, bonusLabel string) (gifURL string, commentID int64, err error) {
	var opts []slots.Option
	if bonusLabel != "" {
		opts = append(opts, slots.WithBonus(bonusLabel))
	}
	gif, err := slots.Generate(entries, chosen, RandSeed(), opts...)
	if err != nil {
		return "", 0, fmt.Errorf("generate gif: %w", err)
	}
	if err := s.Assets.EnsureBranch(s.Branch); err != nil {
		return "", 0, fmt.Errorf("ensure assets branch: %w", err)
	}
	// Timestamp-prefixed so cleanup can prune by age from the name alone.
	path := fmt.Sprintf("%s/%d-%d-%d.gif", AssetDir, time.Now().UTC().Unix(), prNumber, uniq)
	asset, err := s.Assets.PutFile(s.Branch, path, gif, "casino-review: spin asset")
	if err != nil {
		return "", 0, fmt.Errorf("upload gif: %w", err)
	}
	commentID, err = s.GH.CreateComment(prNumber, templates.SpinGIF(asset.DownloadURL))
	if err != nil {
		return asset.DownloadURL, 0, fmt.Errorf("post spin comment: %w", err)
	}
	return asset.DownloadURL, commentID, nil
}
