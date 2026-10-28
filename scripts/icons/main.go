// Command icons draws Capy's icons: a capybara's head, as the menu bar's
// template image and on the app icon. Run it from the repository root after
// changing the drawing:
//
//	go run ./scripts/icons
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

// shape reports what covers the point (x, y) of the unit square.
type shape func(x, y float64) bool

func circle(cx, cy, r float64) shape {
	return func(x, y float64) bool { return math.Hypot(x-cx, y-cy) <= r }
}

func roundRect(x0, y0, x1, y1, r float64) shape {
	return func(x, y float64) bool {
		dx := math.Max(math.Max(x0+r-x, x-(x1-r)), 0)
		dy := math.Max(math.Max(y0+r-y, y-(y1-r)), 0)
		return x >= x0 && x <= x1 && y >= y0 && y <= y1 && math.Hypot(dx, dy) <= r
	}
}

// The head, from the front: a tall rounded box with small ears set high,
// eyes near the top and a broad, blunt muzzle with slit nostrils.
var (
	ears     = []shape{circle(0.3, 0.17, 0.07), circle(0.7, 0.17, 0.07)}
	head     = roundRect(0.22, 0.14, 0.78, 0.92, 0.16)
	snout    = roundRect(0.22, 0.5, 0.78, 0.92, 0.16)
	eyes     = []shape{circle(0.37, 0.36, 0.04), circle(0.63, 0.36, 0.04)}
	nostrils = []shape{roundRect(0.36, 0.6, 0.45, 0.64, 0.02), roundRect(0.55, 0.6, 0.64, 0.64, 0.02)}
)

func anyOf(shapes []shape, x, y float64) bool {
	for _, s := range shapes {
		if s(x, y) {
			return true
		}
	}
	return false
}

// paint returns the color at (x, y) of a drawing in the unit square.
type paint func(x, y float64) color.NRGBA

// render draws p into a size×size image, 4×4 samples per pixel.
func render(size int, p paint) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const n = 4
	for py := range size {
		for px := range size {
			var r, g, b, a float64
			for sy := range n {
				for sx := range n {
					c := p((float64(px)+(float64(sx)+0.5)/n)/float64(size), (float64(py)+(float64(sy)+0.5)/n)/float64(size))
					al := float64(c.A) / 255
					r, g, b, a = r+float64(c.R)*al, g+float64(c.G)*al, b+float64(c.B)*al, a+al
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{R: uint8(r / a), G: uint8(g / a), B: uint8(b / a), A: uint8(a / (n * n) * 255)})
		}
	}
	return img
}

// template is black where the head is, with the eyes and nostrils cut out,
// for macOS to tint in the menu bar.
func template(x, y float64) color.NRGBA {
	if (head(x, y) || anyOf(ears, x, y)) && !anyOf(eyes, x, y) && !anyOf(nostrils, x, y) {
		return color.NRGBA{A: 255}
	}
	return color.NRGBA{}
}

var (
	cream = color.NRGBA{R: 0xf4, G: 0xe9, B: 0xdc, A: 0xff}
	fur   = color.NRGBA{R: 0x9a, G: 0x6b, B: 0x45, A: 0xff}
	ear   = color.NRGBA{R: 0x7a, G: 0x52, B: 0x33, A: 0xff}
	muzz  = color.NRGBA{R: 0x86, G: 0x5c, B: 0x3a, A: 0xff}
	dark  = color.NRGBA{R: 0x2b, G: 0x1d, B: 0x14, A: 0xff}
)

// appIcon is the head on a cream rounded square, inset as macOS icons are.
func appIcon(x, y float64) color.NRGBA {
	const inset = 100.0 / 1024
	if !roundRect(inset, inset, 1-inset, 1-inset, 0.18)(x, y) {
		return color.NRGBA{}
	}
	// The head in the middle 70% of the square.
	hx, hy := (x-0.15)/0.7, (y-0.12)/0.7
	switch {
	case anyOf(eyes, hx, hy) || anyOf(nostrils, hx, hy):
		return dark
	case snout(hx, hy):
		return muzz
	case head(hx, hy):
		return fur
	case anyOf(ears, hx, hy):
		return ear
	}
	return cream
}

func write(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	write("internal/views/tray.png", render(36, template))
	write("internal/views/icon.png", render(192, appIcon)) // the welcome's, 96 DIPs
	if err := os.MkdirAll("resources", 0o755); err != nil {
		log.Fatal(err)
	}
	write("resources/icon.png", render(1024, appIcon))
}
