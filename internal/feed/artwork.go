package feed

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// ArtworkSize is the artwork's width and height in pixels (Apple asks for
// 1400-3000).
const ArtworkSize = 1400

// Artwork returns the channel artwork as PNG: a waveform on a dark
// background, drawn in code so the repository holds no binary assets. The
// result is shared; callers must not modify it.
func Artwork() []byte { return artwork() }

var artwork = sync.OnceValue(func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, ArtworkSize, ArtworkSize))
	bg := color.RGBA{0x11, 0x18, 0x27, 0xff}
	fg := color.RGBA{0xf5, 0x9e, 0x0b, 0xff}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = bg.R, bg.G, bg.B, bg.A
	}
	const (
		bars   = 23
		barW   = 36
		gap    = 18
		maxH   = 760.0
		minH   = 60.0
		center = ArtworkSize / 2
	)
	left := (ArtworkSize - (bars*barW + (bars-1)*gap)) / 2
	for i := range bars {
		// A speech-like envelope: tall in the middle, varying bar to bar.
		x := float64(i)/float64(bars-1)*2 - 1
		h := minH + (maxH-minH)*math.Cos(x*math.Pi/2)*(0.55+0.45*math.Abs(math.Sin(float64(i)*1.7)))
		x0 := left + i*(barW+gap)
		fillRounded(img, x0, center-int(h/2), barW, int(h), fg)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err) // encoding an in-memory RGBA image can't fail
	}
	return b.Bytes()
})

// fillRounded fills a w×h rectangle at (x, y) whose short ends are
// semicircles.
func fillRounded(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	r := float64(w) / 2
	for py := y; py < y+h; py++ {
		for px := x; px < x+w; px++ {
			cx := float64(px-x) + 0.5
			cy := float64(py-y) + 0.5
			// Distance to the nearest point of the bar's center line.
			dy := 0.0
			if cy < r {
				dy = r - cy
			} else if cy > float64(h)-r {
				dy = cy - (float64(h) - r)
			}
			if dx := cx - r; dx*dx+dy*dy <= r*r {
				img.SetRGBA(px, py, c)
			}
		}
	}
}
