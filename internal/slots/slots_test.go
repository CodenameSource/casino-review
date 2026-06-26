package slots

import (
	"bytes"
	"image/gif"
	"testing"
)

// planSteps must always sum exactly to the distance, never stall (every step >=1),
// and therefore land precisely on the target — this is what makes the integrator
// both stutter-free (no duplicate scrolls) and exact-landing.
func TestPlanStepsExactAndMonotonic(t *testing.T) {
	cases := []struct {
		dist, vMax int
		rate       float64
	}{
		{1, 30, 0.86}, {2, 14, 0.82}, {60, 14, 0.82}, {120, 16, 0.82},
		{1920, 30, 0.86}, {2400, 30, 0.86}, {37, 30, 0.86}, {240, 14, 0.8},
	}
	for _, c := range cases {
		steps := planSteps(c.dist, c.vMax, c.rate)
		sum := 0
		for i, s := range steps {
			if s < 1 {
				t.Fatalf("dist=%d: step %d is %d (<1) — would freeze a frame", c.dist, i, s)
			}
			sum += s
		}
		if sum != c.dist {
			t.Fatalf("dist=%d: steps sum to %d, not the distance", c.dist, sum)
		}
	}
}

// Decode the produced GIF and assert the visible-behaviour invariants:
//   - no two consecutive frames are pixel-identical (a frozen frame == stutter);
//   - the winning gold/white reveal colours never appear before the reveal
//     (no spoiler / no guessable tell from colour);
//   - generation never panics for any review count or seed (strip bounds).
func TestGifInvariants(t *testing.T) {
	reviewSets := [][]string{
		{"a-review", "b-review"},
		{"tsetso-review", "dimoreview", "gigareview"},
		{"one", "two", "three", "four", "five"},
	}
	for _, reviews := range reviewSets {
		for seed := int64(0); seed < 8; seed++ {
			for idx := range reviews {
				data, err := Generate(reviews, idx, seed*7+int64(idx))
				if err != nil {
					t.Fatalf("reviews=%d seed=%d idx=%d: %v", len(reviews), seed, idx, err)
				}
				g, err := gif.DecodeAll(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("decode reviews=%d seed=%d idx=%d: %v", len(reviews), seed, idx, err)
				}

				// No frozen frames.
				for i := 1; i < len(g.Image); i++ {
					if bytes.Equal(g.Image[i-1].Pix, g.Image[i].Pix) {
						t.Fatalf("reviews=%d seed=%d idx=%d: frames %d and %d identical (stutter)",
							len(reviews), seed, idx, i-1, i)
					}
				}

				// No reveal colours before the reveal. idx 7 (bright white band/
				// border) first appears exactly at the reveal; idx 4 (winner gold)
				// must not appear before that.
				firstReveal := len(g.Image)
				for i, img := range g.Image {
					if bytes.IndexByte(img.Pix, 7) >= 0 {
						firstReveal = i
						break
					}
				}
				if firstReveal == len(g.Image) {
					t.Fatalf("reviews=%d seed=%d idx=%d: never revealed", len(reviews), seed, idx)
				}
				for i := 0; i < firstReveal; i++ {
					if bytes.IndexByte(g.Image[i].Pix, 4) >= 0 {
						t.Fatalf("reviews=%d seed=%d idx=%d: gold (winner) visible at frame %d, before reveal %d",
							len(reviews), seed, idx, i, firstReveal)
					}
				}
				// The reveal must come well after the start (i.e. there was a spin).
				if firstReveal < 20 {
					t.Fatalf("reviews=%d seed=%d idx=%d: reveal at frame %d is too early (no real spin)",
						len(reviews), seed, idx, firstReveal)
				}
			}
		}
	}
}
