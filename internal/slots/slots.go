// Package slots renders a "slot machine" GIF that spins through a set of review
// names, teases a random number of near-misses (sometimes none, like a real
// reel), then dramatically reveals the winner.
//
// Design notes (why it looks the way it does):
//   - Frames are drawn natively at 400x300 so the reel can move in 1-pixel steps.
//   - Motion is driven by a velocity integrator (see planSteps) that always
//     advances >=1px per frame and clamps the final step to land exactly on the
//     target. That guarantees no two consecutive frames share a scroll value,
//     which is what kills the "frozen frame then jump" stutter.
//   - Per-frame delay is capped as the reel slows, so the tail feels deliberate
//     rather than laggy.
//   - The winner is drawn as plain white until the reveal; the false stops are
//     visually identical to the real landing until the gold flash fires, so a
//     viewer can't predict the result from position, dwell time, or timing.
package slots

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"math"
	"math/rand"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Frame geometry (native resolution — no upscale step).
const (
	W           = 400
	H           = 300
	rowH        = 60
	visibleRows = 5
	centerRow   = 2
	centerY     = centerRow * rowH // 120
	textScale   = 2                // bitmap font is drawn at this integer scale
)

// Palette indices are written directly for flat fills; the grey ramp (10-13) is
// only ever reached via nearest-match so the fades have smooth intermediate tones.
var palette = color.Palette{
	color.RGBA{0x12, 0x12, 0x1c, 0xff}, // 0 background
	color.RGBA{0x22, 0x22, 0x32, 0xff}, // 1 reel panel
	color.RGBA{0x5a, 0x46, 0x14, 0xff}, // 2 highlight band
	color.RGBA{0xf0, 0xf0, 0xf5, 0xff}, // 3 text
	color.RGBA{0xfa, 0xd2, 0x50, 0xff}, // 4 winning / accent gold
	color.RGBA{0xc8, 0xa0, 0x28, 0xff}, // 5 border
	color.RGBA{0x6e, 0x6e, 0x8c, 0xff}, // 6 dim text
	color.RGBA{0xff, 0xff, 0xff, 0xff}, // 7 bright white (flash / sparkle)
	color.RGBA{0xf0, 0x5a, 0x78, 0xff}, // 8 hot pink (flash)
	color.RGBA{0x84, 0x32, 0x0a, 0xff}, // 9 banner shadow
	color.RGBA{0x3a, 0x3a, 0x48, 0xff}, // 10-13 grey ramp (fades only)
	color.RGBA{0x5e, 0x5e, 0x70, 0xff},
	color.RGBA{0x8c, 0x8c, 0xa0, 0xff},
	color.RGBA{0xc0, 0xc0, 0xd0, 0xff},
}

var bg = palette[0].(color.RGBA)

// frame captures everything variable about a single rendered frame.
type frame struct {
	scroll      int         // reel offset in px (the centred strip index is (centerY-scroll)/rowH)
	band        color.Color // centre highlight band colour
	reveal      bool        // draw the winning row gold + bold (the only winner tell)
	sparkleSeed int         // >0 scatters twinkles
	border      color.Color // non-nil draws a pulsing outline around the band
	borderThick int         // outline thickness in px
	banner      bool        // draw the WINNER banner across the top
	fade        float64     // 0 = full colour, 1 = faded to background (loop seam)
}

// builder accumulates frames + delays and shares the palette-mapping memo.
type builder struct {
	white  []*image.RGBA // one cached text image per review (plain)
	gold   *image.RGBA   // the winner's name in gold
	banner *image.RGBA   // "* WINNER *"
	strip  []int         // review index shown at each reel position
	csi    int           // strip index of the winner
	memo   map[color.RGBA]uint8

	frames []*image.Paletted
	delays []int
}

func (b *builder) add(f frame, delayCs int) {
	b.frames = append(b.frames, b.render(f))
	b.delays = append(b.delays, delayCs)
}

// Generate produces the animated GIF. chosenIdx is the selector's winner and is
// always the final landing; seed only varies the decoys, offsets, and timing.
func Generate(reviews []string, chosenIdx int, seed int64) ([]byte, error) {
	r := rand.New(rand.NewSource(seed))

	// Randomise the spin so every pull feels different — the casino experience.
	// csi sets how far/long it blurs before slowing; spinV the blur speed; spinRate
	// how late and how gently it eases to a stop (higher = a longer, more
	// suspenseful glide; lower = a snappier stop).
	csi := 28 + r.Intn(24)                // winner position: spin length 28-51 entries
	spinV := 26 + r.Intn(10)              // cruise speed 26-35 px/frame
	spinRate := 0.875 + r.Float64()*0.045 // 0.875-0.92 deceleration gentleness

	// Endgame, like a real reel. Near-miss stops: the reel approaches the winner
	// from above (teases, max 3 — often none, rarely 3) and may also overshoot
	// past it and tick back up (overshoot, max 2 — one less, its top value just as
	// rare, mirroring the approach).
	teaseWeights := []int{0, 0, 1, 1, 1, 2, 2, 3}
	overWeights := []int{0, 0, 1, 1, 1, 2}
	teases := teaseWeights[r.Intn(len(teaseWeights))]
	overshoot := overWeights[r.Intn(len(overWeights))]

	// The entries the reel stops on, ending on the winner: approach from above,
	// then (if it overshoots) blow past the winner and tick back up onto it.
	path := make([]int, 0, teases+overshoot+1)
	for j := csi - teases; j < csi; j++ { // approach from above: csi-teases .. csi-1
		path = append(path, j)
	}
	for j := csi + overshoot; j > csi; j-- { // overshoot below, then tick back up
		path = append(path, j)
	}
	path = append(path, csi) // settle on the winner

	maxBelow := visibleRows - 1 - centerRow
	stripLen := csi + maxBelow + 3 // room for up to a +2 overshoot below the winner
	strip := make([]int, stripLen)
	for i := range strip {
		strip[i] = r.Intn(len(reviews))
	}
	strip[csi] = chosenIdx
	// Every near-miss stop should show a name other than the winner so the landing
	// reads as a real change.
	setDecoy := func(idx int) {
		if idx >= 0 && idx < stripLen && idx != csi && len(reviews) > 1 {
			strip[idx] = (chosenIdx + 1 + r.Intn(len(reviews)-1)) % len(reviews)
		}
	}
	for _, idx := range path[:len(path)-1] {
		setDecoy(idx)
	}

	b := &builder{
		white:  make([]*image.RGBA, len(reviews)),
		strip:  strip,
		csi:    csi,
		memo:   make(map[color.RGBA]uint8, 64),
		gold:   textImage(strings.ToUpper(reviews[chosenIdx]), palette[4]),
		banner: textImage("* WINNER *", palette[7]),
	}
	for i, name := range reviews {
		b.white[i] = textImage(strings.ToUpper(name), palette[3])
	}

	scrollFor := func(idx int) int { return centerY - idx*rowH }
	cur := 0
	// moveTo glides the reel onto entry idx through the velocity integrator, so
	// no two consecutive frames share a scroll value (no stutter) and it lands
	// exactly on the target.
	moveTo := func(idx, vMax int, rate float64) {
		target := scrollFor(idx)
		for _, step := range planSteps(abs(target-cur), vMax, rate) {
			if target < cur {
				cur -= step
			} else {
				cur += step
			}
			b.add(frame{scroll: cur, band: palette[2]}, delayFor(step, vMax))
		}
		cur = target // exact landing (planSteps already sums to the distance)
	}

	// 1. Fade in from black, then a held beat — a smooth, unhurried start. The
	//    steps are coarse enough that each frame palettises distinctly (tiny
	//    blend deltas near black would otherwise quantise to identical frames).
	for i := 0; i < 5; i++ {
		b.add(frame{scroll: 0, band: palette[2], fade: 1 - float64(i)/5}, 6)
	}
	b.add(frame{scroll: 0, band: palette[2]}, 50)

	// 2. The spin and stops. The first move is the fast spin coming to rest on a
	//    near-miss entry; any remaining move is a slow single-entry tick onto the
	//    winner. Every stop's pause is plain white (the winner only differs once
	//    the reveal fires), and the winner gets the longest, climactic beat.
	prev := 0 // the reel starts at the top (index 0)
	for n, idx := range path {
		vMax, rate := 10, 0.88 // a slow single-entry tick
		switch {
		case n == 0:
			vMax, rate = spinV, spinRate // the fast spin, slowing at a random moment
		case abs(idx-prev) > 1:
			vMax, rate = 18, 0.86 // a quicker glide when blowing past the winner
		}
		moveTo(idx, vMax, rate)
		hold := 26 + n*10
		if n == len(path)-1 {
			hold = 60 // the climactic beat before the reveal
		} else if hold > 52 {
			hold = 52
		}
		b.delays[len(b.delays)-1] = hold
		prev = idx
	}

	// 3. The reveal — gold winner, flashing band, sparkles, pulsing border, banner.
	//    This is the first and only frame where the winner differs from a decoy.
	flash := []color.Color{palette[7], palette[8], palette[4], palette[7], palette[2]}
	winScroll := scrollFor(csi)
	for i := 0; i < 10; i++ {
		thick := 2 + int(4*math.Abs(math.Sin(float64(i)*0.7)))
		b.add(frame{
			scroll: winScroll, band: flash[i%len(flash)], reveal: true,
			sparkleSeed: i + 1, border: palette[7], borderThick: thick, banner: i >= 3,
		}, 9)
	}

	// 4. Hold on the result so it can be read.
	b.add(frame{scroll: winScroll, band: palette[2], reveal: true, banner: true}, 300)

	// 5. Fade out to black; the loop back to step 1 is a clean cross-fade.
	for i := 1; i <= 5; i++ {
		b.add(frame{scroll: winScroll, band: palette[2], reveal: true, banner: true, fade: float64(i) / 5}, 6)
	}

	g := &gif.GIF{Image: b.frames, Delay: b.delays, LoopCount: 0}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// planSteps returns per-frame pixel steps that sum exactly to dist, accelerating
// smoothly to vMax, cruising, then decelerating geometrically to a 1px landing.
// Every step is >=1 (no duplicate frames) and the last step clamps to land exact.
//
// Deceleration is latched: once the reel begins to slow it only slows, so the
// approach is a clean monotonic ease-out (never speeding back up) — the natural
// way a real reel comes to rest.
func planSteps(dist, vMax int, rate float64) []int {
	if dist <= 0 {
		return nil
	}
	var steps []int
	pos := 0
	v := 0.0
	accel := float64(vMax) / 6 // ~6 frames to reach cruise → soft start
	slowing := false
	for pos < dist {
		rem := dist - pos
		if !slowing && stoppingDist(v, rate) >= float64(rem) {
			slowing = true
		}
		if slowing {
			v *= rate
		} else if v < float64(vMax) {
			v += accel
			if v > float64(vMax) {
				v = float64(vMax)
			}
		}
		step := int(math.Round(v))
		// Floor at 2px while slowing so the reel clicks into place instead of an
		// endless 1px crawl; the final clamp below still lands exactly.
		if slowing {
			if step < 2 {
				step = 2
			}
		} else if step < 1 {
			step = 1
		}
		if step > rem {
			step = rem
		}
		steps = append(steps, step)
		pos += step
	}
	return steps
}

// stoppingDist is how far the reel travels if it decelerates from v right now.
func stoppingDist(v, rate float64) float64 {
	d := 0.0
	for v > 1 {
		d += v
		v *= rate
	}
	return d + v
}

// delayFor maps step size to a centisecond delay: fast → snappy, slow → held,
// capped so the slow tail never drags into a slideshow.
func delayFor(step, vMax int) int {
	d := 2 + int(math.Round(9*(1-float64(step)/float64(vMax))))
	if d < 2 {
		d = 2
	}
	if d > 11 {
		d = 11
	}
	return d
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// render draws one frame and palettises it.
func (b *builder) render(f frame) *image.Paletted {
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	fillRect(img, 0, 0, W, H, palette[0])
	fillRect(img, 16, 0, W-16, H, palette[1])

	// Centre highlight band with gold edges.
	fillRect(img, 16, centerY, W-16, centerY+rowH, f.band)
	fillRect(img, 16, centerY-2, W-16, centerY+2, palette[5])
	fillRect(img, 16, centerY+rowH-2, W-16, centerY+rowH+2, palette[5])

	// Reel entries.
	for k := 0; k < len(b.strip); k++ {
		yTop := k*rowH + f.scroll
		if yTop <= -rowH || yTop >= H {
			continue
		}
		cy := yTop + rowH/2
		if f.reveal && k == b.csi {
			blitText(img, b.gold, W/2, cy)
			blitText(img, b.gold, W/2+2, cy) // doubled = bolder
			continue
		}
		blitText(img, b.white[b.strip[k]], W/2, cy)
	}

	if f.border != nil && f.borderThick > 0 {
		drawOutline(img, 12, centerY-6, W-12, centerY+rowH+6, f.borderThick, f.border)
	}
	if f.sparkleSeed > 0 {
		sr := rand.New(rand.NewSource(int64(f.sparkleSeed) * 2654435761))
		for n := 0; n < 12; n++ {
			x := 24 + sr.Intn(W-48)
			y := centerY - 12 + sr.Intn(rowH+24)
			c := palette[7]
			if n%2 == 0 {
				c = palette[4]
			}
			drawSparkle(img, x, y, c)
		}
	}
	if f.banner {
		fillRect(img, 16, 4, W-16, 34, palette[9])
		fillRect(img, 16, 2, W-16, 6, palette[4])
		blitText(img, b.banner, W/2, 19)
	}
	if f.fade > 0 {
		applyFade(img, f.fade)
	}
	return b.paletted(img)
}

// textImage rasterises s once at 1x (white-on-transparent) for later upscaling.
func textImage(s string, col color.Color) *image.RGBA {
	face := basicfont.Face7x13
	d := &font.Drawer{Face: face}
	w := d.MeasureString(s).Ceil()
	img := image.NewRGBA(image.Rect(0, 0, w, face.Height))
	d.Dst = img
	d.Src = &image.Uniform{col}
	d.Dot = fixed.P(0, face.Ascent)
	d.DrawString(s)
	return img
}

// blitText nearest-neighbour upscales a cached text image, centred at (cx, cy).
func blitText(dst *image.RGBA, src *image.RGBA, cx, cy int) {
	dw := src.Bounds().Dx() * textScale
	dh := src.Bounds().Dy() * textScale
	r := image.Rect(cx-dw/2, cy-dh/2, cx-dw/2+dw, cy-dh/2+dh)
	xdraw.NearestNeighbor.Scale(dst, r, src, src.Bounds(), xdraw.Over, nil)
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	xdraw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{c}, image.Point{}, xdraw.Src)
}

func drawOutline(img *image.RGBA, x0, y0, x1, y1, t int, c color.Color) {
	fillRect(img, x0, y0, x1, y0+t, c)
	fillRect(img, x0, y1-t, x1, y1, c)
	fillRect(img, x0, y0, x0+t, y1, c)
	fillRect(img, x1-t, y0, x1, y1, c)
}

func drawSparkle(img *image.RGBA, x, y int, c color.Color) {
	fillRect(img, x-4, y-1, x+5, y+1, c) // horizontal
	fillRect(img, x-1, y-4, x+1, y+5, c) // vertical
}

// applyFade blends every pixel toward the background colour by fraction f.
func applyFade(img *image.RGBA, f float64) {
	if f > 1 {
		f = 1
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			c.R = uint8(float64(c.R)*(1-f) + float64(bg.R)*f)
			c.G = uint8(float64(c.G)*(1-f) + float64(bg.G)*f)
			c.B = uint8(float64(c.B)*(1-f) + float64(bg.B)*f)
			img.SetRGBA(x, y, c)
		}
	}
}

// paletted maps an RGBA frame to the palette, memoising nearest-colour lookups.
func (b *builder) paletted(src *image.RGBA) *image.Paletted {
	dst := image.NewPaletted(image.Rect(0, 0, W, H), palette)
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			c := src.RGBAAt(x, y)
			idx, ok := b.memo[c]
			if !ok {
				idx = uint8(palette.Index(c))
				b.memo[c] = idx
			}
			dst.SetColorIndex(x, y, idx)
		}
	}
	return dst
}
