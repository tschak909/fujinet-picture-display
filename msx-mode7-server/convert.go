package main

import (
	"image"
	"image/color"
	"math"
	"sort"

	xdraw "golang.org/x/image/draw"
)

const (
	screenW   = 512
	screenH   = 212
	satFactor = 2.0
	gammaExp  = 0.70
	brightMul = 1.30
)

// v9938Color is one V9938 palette entry: 3 bits per channel (0–7).
type v9938Color struct{ r, g, b uint8 }

// toRGBA expands 3-bit channels to 8-bit for display / distance math.
func (c v9938Color) toRGBA() color.RGBA {
	e := func(v uint8) uint8 { return uint8(uint16(v) * 255 / 7) }
	return color.RGBA{e(c.r), e(c.g), e(c.b), 255}
}

// toBytes returns the two bytes written to a V9938 palette register.
//
//	byte 0: 0|R2|R1|R0|0|B2|B1|B0
//	byte 1: 0|0|0|0|0|G2|G1|G0
func (c v9938Color) toBytes() (byte, byte) {
	return (c.r << 4) | c.b, c.g
}

func quantize3(v uint8) uint8 { return v >> 5 }

func colorDistV(r, g, b uint8, c v9938Color) float64 {
	e := func(v uint8) float64 { return float64(v) * 255.0 / 7.0 }
	dr := float64(r) - e(c.r)
	dg := float64(g) - e(c.g)
	db := float64(b) - e(c.b)
	return 0.299*dr*dr + 0.587*dg*dg + 0.114*db*db
}

func nearestPal(r, g, b uint8, pal []v9938Color) int {
	best, bestD := 0, math.MaxFloat64
	for i, c := range pal {
		if d := colorDistV(r, g, b, c); d < bestD {
			bestD = d
			best = i
		}
	}
	return best
}

type colorEntry struct{ r, g, b uint8; count int }

func chanVal(e colorEntry, axis int) uint8 {
	switch axis {
	case 0:
		return e.r
	case 1:
		return e.g
	default:
		return e.b
	}
}

// medianCutPalette derives 16 palette entries from the image by running
// median-cut on the V9938 9-bit (3R-3G-3B) color histogram.
func medianCutPalette(img *image.RGBA) []v9938Color {
	var freq [512]int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			key := int(quantize3(c.R))<<6 | int(quantize3(c.G))<<3 | int(quantize3(c.B))
			freq[key]++
		}
	}

	entries := make([]colorEntry, 0, 512)
	for key := 0; key < 512; key++ {
		if freq[key] > 0 {
			entries = append(entries, colorEntry{
				r:     uint8((key >> 6) & 7),
				g:     uint8((key >> 3) & 7),
				b:     uint8(key & 7),
				count: freq[key],
			})
		}
	}

	type bucket []colorEntry
	buckets := []bucket{entries}

	axisWeights := [3]float64{0.299, 0.587, 0.114}

	for len(buckets) < 16 {
		maxScore, splitIdx, splitAxis := -1.0, 0, 0
		for i, bkt := range buckets {
			if len(bkt) <= 1 {
				continue
			}
			for axis := 0; axis < 3; axis++ {
				mn, mx := uint8(7), uint8(0)
				for _, e := range bkt {
					v := chanVal(e, axis)
					if v < mn {
						mn = v
					}
					if v > mx {
						mx = v
					}
				}
				score := float64(mx-mn) * axisWeights[axis]
				if score > maxScore {
					maxScore, splitIdx, splitAxis = score, i, axis
				}
			}
		}
		if maxScore <= 0 {
			break
		}

		bkt := buckets[splitIdx]
		axis := splitAxis
		sort.Slice(bkt, func(i, j int) bool {
			return chanVal(bkt[i], axis) < chanVal(bkt[j], axis)
		})

		// Split at the pixel-count midpoint.
		total, cum := 0, 0
		for _, e := range bkt {
			total += e.count
		}
		half := total / 2
		mid := len(bkt) - 1
		for i, e := range bkt {
			cum += e.count
			if cum >= half {
				mid = i + 1
				break
			}
		}
		if mid <= 0 {
			mid = 1
		}
		if mid >= len(bkt) {
			mid = len(bkt) - 1
		}

		buckets[splitIdx] = bkt[:mid]
		buckets = append(buckets, bkt[mid:])
	}

	pal := make([]v9938Color, 16)
	for i, bkt := range buckets {
		if i >= 16 || len(bkt) == 0 {
			continue
		}
		var sr, sg, sb, total int
		for _, e := range bkt {
			sr += int(e.r) * e.count
			sg += int(e.g) * e.count
			sb += int(e.b) * e.count
			total += e.count
		}
		if total > 0 {
			pal[i] = v9938Color{uint8(sr / total), uint8(sg / total), uint8(sb / total)}
		}
	}
	return pal
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func rgbToHSV(r, g, b uint8) (h, s, v float64) {
	rf := float64(r) / 255
	gf := float64(g) / 255
	bf := float64(b) / 255
	mx := math.Max(rf, math.Max(gf, bf))
	mn := math.Min(rf, math.Min(gf, bf))
	delta := mx - mn
	v = mx
	if mx > 0 {
		s = delta / mx
	}
	if delta > 0 {
		switch mx {
		case rf:
			h = 60 * math.Mod((gf-bf)/delta, 6)
		case gf:
			h = 60 * ((bf-rf)/delta + 2)
		default:
			h = 60 * ((rf-gf)/delta + 4)
		}
		if h < 0 {
			h += 360
		}
	}
	return
}

func hsvToRGB(h, s, v float64) (uint8, uint8, uint8) {
	clamp := func(f float64) uint8 {
		if f <= 0 {
			return 0
		}
		if f >= 1 {
			return 255
		}
		return uint8(f * 255)
	}
	if s == 0 {
		c := clamp(v)
		return c, c, c
	}
	h /= 60
	i := int(h)
	f := h - float64(i)
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	switch i % 6 {
	case 0:
		return clamp(v), clamp(t), clamp(p)
	case 1:
		return clamp(q), clamp(v), clamp(p)
	case 2:
		return clamp(p), clamp(v), clamp(t)
	case 3:
		return clamp(p), clamp(q), clamp(v)
	case 4:
		return clamp(t), clamp(p), clamp(v)
	default:
		return clamp(v), clamp(p), clamp(q)
	}
}

func enhance(img *image.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			h, s, v := rgbToHSV(c.R, c.G, c.B)
			v = math.Pow(v, gammaExp)
			v = math.Min(1, v*brightMul)
			s = math.Min(1, s*satFactor)
			r, g, bv := hsvToRGB(h, s, v)
			img.SetRGBA(x, y, color.RGBA{r, g, bv, c.A})
		}
	}
}

func resizeTo512x212(src image.Image) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, screenW, screenH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

// ConvertToV9938 converts src to V9938 SCREEN 7 (Graphics Mode 6) format.
//
// Binary layout of the returned data slice:
//
//	Bytes    0–31 : 16 palette entries (2 bytes each, V9938 register format)
//	               entry byte 0: 0|R2|R1|R0|0|B2|B1|B0
//	               entry byte 1: 0|0|0|0|0|G2|G1|G0
//	Bytes 32–54303: bitmap (512×212, 4bpp, 2 pixels per byte)
//	               high nibble = left pixel, low nibble = right pixel
//
// Total: 54304 bytes.
func ConvertToV9938(src image.Image) (data []byte, preview *image.RGBA) {
	resized := resizeTo512x212(src)
	enhance(resized)

	pal := medianCutPalette(resized)

	// Encode palette (32 bytes).
	palBytes := make([]byte, 32)
	for i, c := range pal {
		b0, b1 := c.toBytes()
		palBytes[i*2] = b0
		palBytes[i*2+1] = b1
	}

	// Floyd-Steinberg dithering into 4bpp bitmap.
	bitmap := make([]byte, screenW*screenH/2)
	errR := make([]float64, screenW*screenH)
	errG := make([]float64, screenW*screenH)
	errB := make([]float64, screenW*screenH)

	preview = image.NewRGBA(image.Rect(0, 0, screenW, screenH))

	palRGBA := make([]color.RGBA, 16)
	for i, c := range pal {
		palRGBA[i] = c.toRGBA()
	}

	for y := 0; y < screenH; y++ {
		for x := 0; x < screenW; x++ {
			idx := y*screenW + x
			c := resized.RGBAAt(x, y)

			r := clampF(float64(c.R) + errR[idx])
			g := clampF(float64(c.G) + errG[idx])
			b := clampF(float64(c.B) + errB[idx])

			pi := nearestPal(uint8(r), uint8(g), uint8(b), pal)
			pc := palRGBA[pi]

			er := r - float64(pc.R)
			eg := g - float64(pc.G)
			eb := b - float64(pc.B)

			// Distribute error: right 7/16, below-left 3/16, below 5/16, below-right 1/16.
			if x+1 < screenW {
				errR[idx+1] += er * 7 / 16
				errG[idx+1] += eg * 7 / 16
				errB[idx+1] += eb * 7 / 16
			}
			if y+1 < screenH {
				if x-1 >= 0 {
					errR[(y+1)*screenW+x-1] += er * 3 / 16
					errG[(y+1)*screenW+x-1] += eg * 3 / 16
					errB[(y+1)*screenW+x-1] += eb * 3 / 16
				}
				errR[(y+1)*screenW+x] += er * 5 / 16
				errG[(y+1)*screenW+x] += eg * 5 / 16
				errB[(y+1)*screenW+x] += eb * 5 / 16
				if x+1 < screenW {
					errR[(y+1)*screenW+x+1] += er * 1 / 16
					errG[(y+1)*screenW+x+1] += eg * 1 / 16
					errB[(y+1)*screenW+x+1] += eb * 1 / 16
				}
			}

			// Pack: high nibble = even x, low nibble = odd x.
			byteIdx := idx / 2
			if x%2 == 0 {
				bitmap[byteIdx] = byte(pi << 4)
			} else {
				bitmap[byteIdx] |= byte(pi & 0x0f)
			}

			preview.SetRGBA(x, y, pc)
		}
	}

	data = append(palBytes, bitmap...)
	return
}
