package main

import (
	"image"
	"image/color"
	"math"

	xdraw "golang.org/x/image/draw"
)

const (
	screenW    = 256
	screenH    = 192
	satFactor  = 2.0
	gammaExp   = 0.70 // < 1.0 brightens midtones; lifts dark skin tones into lighter palette entries
	brightMul  = 1.30 // overall brightness lift applied after gamma
)

// TMS9918A 16-color palette (index 0 = transparent, treated as black).
var palette = [16]color.RGBA{
	{0, 0, 0, 255},       // 0  Transparent
	{0, 0, 0, 255},       // 1  Black
	{62, 184, 73, 255},   // 2  Medium Green
	{116, 208, 125, 255}, // 3  Light Green
	{89, 85, 224, 255},   // 4  Dark Blue
	{128, 118, 241, 255}, // 5  Light Blue
	{185, 94, 81, 255},   // 6  Dark Red
	{101, 219, 239, 255}, // 7  Cyan
	{219, 101, 89, 255},  // 8  Medium Red
	{255, 137, 125, 255}, // 9  Light Red
	{204, 195, 94, 255},  // 10 Dark Yellow
	{222, 208, 135, 255}, // 11 Light Yellow
	{58, 162, 65, 255},   // 12 Dark Green
	{183, 102, 181, 255}, // 13 Magenta
	{204, 204, 204, 255}, // 14 Gray
	{255, 255, 255, 255}, // 15 White
}

func colorDist(r, g, b uint8, c color.RGBA) float64 {
	dr := float64(r) - float64(c.R)
	dg := float64(g) - float64(c.G)
	db := float64(b) - float64(c.B)
	// Perceptual luminance weighting
	return 0.299*dr*dr + 0.587*dg*dg + 0.114*db*db
}

// nearestTMS returns the closest palette index (1–15, never transparent).
func nearestTMS(r, g, b uint8) int {
	best, bestDist := 1, math.MaxFloat64
	for i := 1; i < 16; i++ {
		if d := colorDist(r, g, b, palette[i]); d < bestDist {
			bestDist = d
			best = i
		}
	}
	return best
}

func rgbToHSV(r, g, b uint8) (h, s, v float64) {
	rf := float64(r) / 255
	gf := float64(g) / 255
	bf := float64(b) / 255
	max := math.Max(rf, math.Max(gf, bf))
	min := math.Min(rf, math.Min(gf, bf))
	delta := max - min
	v = max
	if max > 0 {
		s = delta / max
	}
	if delta > 0 {
		switch max {
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
			v = math.Pow(v, gammaExp)      // lift midtones without blowing highlights
			v = math.Min(1, v*brightMul)   // overall brightness boost
			s = math.Min(1, s*satFactor)   // saturate to maximise palette spread
			r, g, bv := hsvToRGB(h, s, v)
			img.SetRGBA(x, y, color.RGBA{r, g, bv, c.A})
		}
	}
}

func resizeTo256x192(src image.Image) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, screenW, screenH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

// ConvertToTMS9918 converts src to TMS9918A Graphics II format.
//
// Output binary layout returned by Data():
//
//	Bytes     0–767   : Pattern Name Table  (sequential 0x00–0xFF × 3)
//	Bytes   768–6911  : Pattern Generator Table  (6144 bytes)
//	Bytes  6912–13055 : Color Table              (6144 bytes)
//
// Each Color Table byte: high nibble = fg color index (1–15),
// low nibble = bg color index (1–15).
// Each Pattern byte bit 7 = leftmost pixel; 1 = fg, 0 = bg.
func ConvertToTMS9918(src image.Image) (nameTable, patternTable, colorTable []byte, preview *image.RGBA) {
	resized := resizeTo256x192(src)
	enhance(resized)

	// Name table: trivially sequential for Graphics II
	nameTable = make([]byte, 768)
	for i := range nameTable {
		nameTable[i] = byte(i % 256)
	}

	patternTable = make([]byte, 6144)
	colorTable = make([]byte, 6144)
	preview = image.NewRGBA(image.Rect(0, 0, screenW, screenH))

	for y := 0; y < screenH; y++ {
		section := y / 64
		rowInChar := y % 8
		charRow := (y % 64) / 8 // 0–7 within section

		for cx := 0; cx < 32; cx++ {
			charIdx := charRow*32 + cx
			offset := section*2048 + charIdx*8 + rowInChar

			// Map all 8 pixels to nearest TMS colors
			var pixR, pixG, pixB [8]uint8
			var tmsIdx [8]int
			var freq [16]int

			for bit := 0; bit < 8; bit++ {
				x := cx*8 + bit
				c := resized.RGBAAt(x, y)
				pixR[bit], pixG[bit], pixB[bit] = c.R, c.G, c.B
				idx := nearestTMS(c.R, c.G, c.B)
				tmsIdx[bit] = idx
				freq[idx]++
			}

			// Pick top-2 most frequent colors
			fg, bg := -1, -1
			for i := 1; i < 16; i++ {
				if fg == -1 || freq[i] > freq[fg] {
					bg = fg
					fg = i
				} else if bg == -1 || freq[i] > freq[bg] {
					bg = i
				}
			}
			if bg == -1 {
				// All 8 pixels are the same color
				if fg == 1 {
					bg = 15
				} else {
					bg = 1
				}
			}

			// Build pattern byte and color byte
			var pattern byte
			for bit := 0; bit < 8; bit++ {
				var useFG bool
				switch tmsIdx[bit] {
				case fg:
					useFG = true
				case bg:
					useFG = false
				default:
					// This pixel's best color wasn't one of the chosen two;
					// pick whichever of fg/bg is closer to the actual pixel RGB.
					dfg := colorDist(pixR[bit], pixG[bit], pixB[bit], palette[fg])
					dbg := colorDist(pixR[bit], pixG[bit], pixB[bit], palette[bg])
					useFG = dfg <= dbg
				}
				if useFG {
					pattern |= 1 << (7 - bit)
				}
			}

			patternTable[offset] = pattern
			colorTable[offset] = byte(fg<<4) | byte(bg)

			// Render preview with actual TMS palette colors
			for bit := 0; bit < 8; bit++ {
				x := cx*8 + bit
				if (pattern>>(7-bit))&1 == 1 {
					preview.SetRGBA(x, y, palette[fg])
				} else {
					preview.SetRGBA(x, y, palette[bg])
				}
			}
		}
	}

	return
}
