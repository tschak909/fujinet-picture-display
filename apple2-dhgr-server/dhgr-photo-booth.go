// dhgr-photo-booth — VCF FujiNet Booth Tool
//
// Accepts a photo upload from a phone, converts it to Apple II Double Hi-Res
// Graphics (DHGR) format: 560×192 pixels, 16 colours, stored across two
// interleaved 8 KB memory pages (auxiliary + main).
//
// Output binary layout (16 384 bytes total):
//   bytes     0– 8191  →  auxiliary page  (BLOAD at $2000 with aux bank selected)
//   bytes  8192–16383  →  main page       (BLOAD at $2000 in main memory)
//
// Endpoints
//
//   GET  /           – mobile-friendly HTML upload form
//   POST /upload     – multipart image upload; shows success page with preview
//   GET  /image      – raw 16 384-byte DHGR binary (aux page then main page)
//   GET  /preview    – 560×192 PNG in true DHGR palette colours
//   GET  /timestamp  – Unix timestamp of last upload (plain integer; 0 if none)
//
// Stdlib only — no external dependencies.

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"html/template"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"
)

// ── DHGR constants ────────────────────────────────────────────────────────────

const (
	// DHGR has 560 physical pixels per row.  Each colour "cell" spans 4
	// physical pixels, giving 140 addressable colour cells per scanline.
	dhgrCellW  = 140  // colour cells per scanline  (560 px / 4)
	dhgrH      = 192  // scanlines
	dhgrAuxSz  = 8192 // auxiliary memory page size in bytes
	dhgrMainSz = 8192 // main memory page size in bytes
)

// Apple II DHGR 16-colour palette (RGB approximations).
// Index values correspond to the 4-bit DHGR colour encoding.
var dhgrPalette = [16]color.NRGBA{
	{R: 0x00, G: 0x00, B: 0x00, A: 0xff}, //  0  Black
	{R: 0xDD, G: 0x00, B: 0x33, A: 0xff}, //  1  Magenta
	{R: 0x00, G: 0x00, B: 0x99, A: 0xff}, //  2  Dark Blue
	{R: 0xAA, G: 0x00, B: 0xFF, A: 0xff}, //  3  Purple
	{R: 0x00, G: 0x77, B: 0x22, A: 0xff}, //  4  Dark Green
	{R: 0x55, G: 0x55, B: 0x55, A: 0xff}, //  5  Dark Gray
	{R: 0x22, G: 0x22, B: 0xFF, A: 0xff}, //  6  Medium Blue
	{R: 0x66, G: 0xAA, B: 0xFF, A: 0xff}, //  7  Light Blue
	{R: 0x88, G: 0x55, B: 0x00, A: 0xff}, //  8  Brown
	{R: 0xFF, G: 0x66, B: 0x00, A: 0xff}, //  9  Orange
	{R: 0xAA, G: 0xAA, B: 0xAA, A: 0xff}, // 10  Light Gray
	{R: 0xFF, G: 0x99, B: 0x88, A: 0xff}, // 11  Pink
	{R: 0x00, G: 0xFF, B: 0x44, A: 0xff}, // 12  Green
	{R: 0xFF, G: 0xFF, B: 0x00, A: 0xff}, // 13  Yellow
	{R: 0x44, G: 0xFF, B: 0xCC, A: 0xff}, // 14  Aqua
	{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xff}, // 15  White
}

// ── shared state ──────────────────────────────────────────────────────────────

var (
	mu         sync.RWMutex
	auxPage    [dhgrAuxSz]byte
	mainPage   [dhgrMainSz]byte
	lastUpload time.Time
	hasImage   bool
)

// ── DHGR interleaved scanline offset ─────────────────────────────────────────

// hgrOffset returns the byte offset within an 8 192-byte page for scanline y.
// Both HGR and DHGR pages use identical non-linear interleaving:
//
//	offset = (y % 8)       × 0x0400
//	       + ((y/8) % 8)   × 0x0080
//	       + (y / 64)      × 0x0028
func hgrOffset(y int) int {
	return (y&7)*0x0400 + ((y>>3)&7)*0x0080 + (y>>6)*0x0028
}

// ── EXIF orientation ─────────────────────────────────────────────────────────

// exifOrientation parses the EXIF orientation tag (0x0112) from raw JPEG
// bytes without any external library.  Returns values 1-8 per the EXIF spec,
// or 1 (normal) if the tag is absent or unreadable.
//
// JPEG APP1 layout:
//   FF E1  <2-byte length>  "Exif\0\0"  <TIFF data>
// TIFF IFD entry layout (per entry, 12 bytes):
//   <2-byte tag>  <2-byte type>  <4-byte count>  <4-byte value-or-offset>
func exifOrientation(data []byte) int {
	// Bail immediately if this isn't a JPEG (magic bytes FF D8).
	// Non-JPEG uploads (PNG, GIF, WebP) have no EXIF and may contain 0xFF
	// bytes that would cause the segment loop to wander through the whole file.
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}

	// Walk JPEG segments looking for APP1 (marker 0xE1) with an Exif payload.
	// Each segment: FF <marker> <2-byte length incl. the length field> <payload>
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			// Not a valid segment start — corrupted or end of header area.
			break
		}
		marker := data[i+1]
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))

		// segLen includes its own 2 bytes; anything smaller is malformed.
		if segLen < 2 {
			break
		}

		// SOS (0xDA) marks the start of compressed image data — no more headers.
		if marker == 0xDA {
			break
		}

		payloadEnd := i + 2 + segLen // exclusive end of this segment in data
		if payloadEnd > len(data) {
			break // truncated file
		}

		if marker == 0xE1 {
			// APP1: check for "Exif\0\0" header (6 bytes after the length field).
			payload := data[i+4 : payloadEnd]
			if len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
				return parseTIFFOrientation(payload[6:])
			}
		}

		i = payloadEnd
	}
	return 1
}

// parseTIFFOrientation reads orientation from a raw TIFF block (little- or
// big-endian).  Returns 1 if the tag is not found.
func parseTIFFOrientation(b []byte) int {
	if len(b) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	// IFD offset is at bytes 4-7.
	ifdOff := int(order.Uint32(b[4:8]))
	if ifdOff+2 > len(b) {
		return 1
	}
	nEntries := int(order.Uint16(b[ifdOff : ifdOff+2]))
	for i := 0; i < nEntries; i++ {
		entry := ifdOff + 2 + i*12
		if entry+12 > len(b) {
			break
		}
		tag := order.Uint16(b[entry : entry+2])
		if tag == 0x0112 { // Orientation
			return int(order.Uint16(b[entry+8 : entry+10]))
		}
	}
	return 1
}

// applyOrientation rotates/flips img to match the EXIF orientation value.
// The Apple II display is landscape, so portrait shots (orientations 5-8)
// are rotated 90° clockwise to fill the 560×192 frame correctly.
//
// EXIF orientation values:
//   1 = normal (top-left)      2 = flip horizontal
//   3 = rotate 180°            4 = flip vertical
//   5 = transpose              6 = rotate 90° CW   (phone portrait, held right)
//   7 = transverse             8 = rotate 90° CCW  (phone portrait, held left)
func applyOrientation(img image.Image, orient int) image.Image {
	b := img.Bounds()
	w, h := b.Max.X-b.Min.X, b.Max.Y-b.Min.Y

	switch orient {
	case 1:
		return img // nothing to do
	case 2: // flip horizontal
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(w-1-x, y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 3: // rotate 180°
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(w-1-x, h-1-y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 4: // flip vertical
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(x, h-1-y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 5: // transpose (flip across top-left diagonal)
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(y, x, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 6: // rotate 90° CW (most common phone portrait)
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(h-1-y, x, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 7: // transverse (flip across top-right diagonal)
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(h-1-y, w-1-x, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	case 8: // rotate 90° CCW (phone portrait, held other way)
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(y, w-1-x, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	}
	return img
}

// ── bilinear scaler ───────────────────────────────────────────────────────────

// bilinearScale resizes src to dstW×dstH using bilinear interpolation.
func bilinearScale(src image.Image, dstW, dstH int) *image.NRGBA {
	sb := src.Bounds()
	srcW := sb.Max.X - sb.Min.X
	srcH := sb.Max.Y - sb.Min.Y
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	ox, oy := sb.Min.X, sb.Min.Y

	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			sx := float64(x) * float64(srcW) / float64(dstW)
			sy := float64(y) * float64(srcH) / float64(dstH)
			x0, y0 := int(sx), int(sy)
			x1, y1 := x0+1, y0+1
			if x1 >= srcW {
				x1 = srcW - 1
			}
			if y1 >= srcH {
				y1 = srcH - 1
			}
			fx, fy := sx-float64(x0), sy-float64(y0)

			// image.RGBA() returns 16-bit (0-65535) pre-multiplied values.
			r00, g00, b00, _ := src.At(ox+x0, oy+y0).RGBA()
			r10, g10, b10, _ := src.At(ox+x1, oy+y0).RGBA()
			r01, g01, b01, _ := src.At(ox+x0, oy+y1).RGBA()
			r11, g11, b11, _ := src.At(ox+x1, oy+y1).RGBA()

			lf := func(a, b, t float64) float64 { return a*(1-t) + b*t }
			r := lf(lf(float64(r00), float64(r10), fx), lf(float64(r01), float64(r11), fx), fy) / 257
			g := lf(lf(float64(g00), float64(g10), fx), lf(float64(g01), float64(g11), fx), fy) / 257
			b := lf(lf(float64(b00), float64(b10), fx), lf(float64(b01), float64(b11), fx), fy) / 257

			dst.SetNRGBA(x, y, color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff})
		}
	}
	return dst
}

// ── palette matching (linear light) ──────────────────────────────────────────

// sRGBToLinear converts a gamma-encoded sRGB component (0â255) to linear-light
// intensity [0.0, 1.0] using the IECÂ 61966-2-1 transfer curve.
// Working in linear space is essential for perceptually correct colour
// quantisation: the sRGB midpoint between DarkÂ Gray (85) and LightÂ Gray (170)
// falls at sRGBÂ 127, but the perceptual midpoint is at sRGBÂ ~139 (linear 0.255).
// Matching in linear moves the dark-gray capture zone from 85Â sRGB wide to
// ~108Â sRGB wide, restoring contrast in shadow regions.
func sRGBToLinear(v float64) float64 {
	v /= 255.0
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// linearPalette is dhgrPalette pre-converted to linear [0.0, 1.0].
// Built once at init so nearestDHGR doesnât repeat the conversion per pixel.
var linearPalette [16][3]float64

func init() {
	for i, c := range dhgrPalette {
		linearPalette[i][0] = sRGBToLinear(float64(c.R))
		linearPalette[i][1] = sRGBToLinear(float64(c.G))
		linearPalette[i][2] = sRGBToLinear(float64(c.B))
	}
}

// nearestDHGR returns the index (0â15) of the closest DHGR palette entry to
// (r, g, b), where all three are linear-light values in [0.0, 1.0].
func nearestDHGR(r, g, b float64) byte {
	best := byte(0)
	bestDist := math.MaxFloat64
	for i, lc := range linearPalette {
		dr := r - lc[0]
		dg := g - lc[1]
		db := b - lc[2]
		if d := dr*dr + dg*dg + db*db; d < bestDist {
			bestDist = d
			best = byte(i)
		}
	}
	return best
}

// ── DHGR conversion ───────────────────────────────────────────────────────────

// convertToDHGR converts src into Apple II DHGR format in three stages:
//
//  1. Scale src to 140×192 — one colour cell per DHGR 4-pixel group.
//  2. Floyd-Steinberg dither in RGB space against the 16-colour DHGR palette.
//  3. Pack colour indices into two interleaved 8 KB pages (aux and main).
//
// DHGR scanline memory layout (80 bytes per scanline, split across aux + main):
//
//	display order:  aux[0]  main[0]  aux[1]  main[1]  …  aux[39]  main[39]
//	each byte:      bits 0-6 = 7 pixel bits;  bit 7 = 0  (required for DHGR)
//
// Each colour cell (4 physical pixels wide) contributes 4 consecutive bits
// to the 560-bit pixel stream, written LSB-first.  Stream byte index sb maps:
//
//	even sb → aux page  at hgrOffset(y) + sb/2
//	odd  sb → main page at hgrOffset(y) + sb/2
func convertToDHGR(src image.Image) (aux [dhgrAuxSz]byte, main [dhgrMainSz]byte) {

	// ── 1. Scale to 140×192 ───────────────────────────────────────────────
	scaled := bilinearScale(src, dhgrCellW, dhgrH)

	// ── 2. Posterize: convert to linear light, apply shadow expansion,
	// nearest-colour quantise — no dithering. ──────────────────────────────
	//
	// Dithering spreads quantisation error across adjacent cells, which looks
	// noisy at DHGR’s coarse 4-pixel cell granularity.  A clean nearest-colour
	// snap produces a bold, poster-like result that reads much better on the
	// actual hardware at normal viewing distance.
	//
	// We still convert to linear light before matching so that the colour
	// thresholds are perceptually correct, and retain the shadow-gamma
	// expansion so dark areas don’t collapse to black.
	type fRGB struct{ r, g, b float64 } // linear [0.0, 1.0]
	buf := make([]fRGB, dhgrCellW*dhgrH)
	for y := 0; y < dhgrH; y++ {
		for x := 0; x < dhgrCellW; x++ {
			c := scaled.NRGBAAt(x, y)
			buf[y*dhgrCellW+x] = fRGB{
				sRGBToLinear(float64(c.R)),
				sRGBToLinear(float64(c.G)),
				sRGBToLinear(float64(c.B)),
			}
		}
	}

	// Brightness lift: exponent < 1 brightens in linear space.
	// 0.75 lifts midtones by ~+25 sRGB units; pure white stays pinned at 1.0.
	const brightnessGamma = 0.75
	for i := range buf {
		buf[i].r = math.Pow(buf[i].r, brightnessGamma)
		buf[i].g = math.Pow(buf[i].g, brightnessGamma)
		buf[i].b = math.Pow(buf[i].b, brightnessGamma)
	}

	// Saturation boost: pull each pixel's colour away from its luminance
	// (BT.709 linear coefficients).  Pure greys are unaffected (chroma = 0);
	// desaturated colours like skin tones, wood, foliage are pushed toward
	// the nearest vivid palette entry instead of collapsing into grey.
	// factor 2.5 doubles the colour separation without over-saturating.
	const satBoost = 2.5
	for i := range buf {
		lum := 0.2126*buf[i].r + 0.7152*buf[i].g + 0.0722*buf[i].b
		buf[i].r = lum + (buf[i].r-lum)*satBoost
		buf[i].g = lum + (buf[i].g-lum)*satBoost
		buf[i].b = lum + (buf[i].b-lum)*satBoost
	}

	clamp := func(v float64) float64 {
		if v < 0 { return 0 }
		if v > 1 { return 1 }
		return v
	}

	// Simple nearest-colour snap — no error diffusion.
	indices := make([]byte, dhgrCellW*dhgrH)
	for y := 0; y < dhgrH; y++ {
		for x := 0; x < dhgrCellW; x++ {
			i := y*dhgrCellW + x
			indices[i] = nearestDHGR(
				clamp(buf[i].r),
				clamp(buf[i].g),
				clamp(buf[i].b),
			)
		}
	}

			// ── 3. Pack colour indices into aux + main pages ──────────────────────
	for y := 0; y < dhgrH; y++ {
		off := hgrOffset(y)

		// Build the 80-byte interleaved pixel stream for this scanline.
		// 7 usable bits × 80 bytes = 560 pixel positions.
		var stream [80]byte

		for cellX := 0; cellX < dhgrCellW; cellX++ {
			c := indices[y*dhgrCellW+cellX]
			for bit := 0; bit < 4; bit++ {
				pixPos := cellX*4 + bit // position in 560-pixel row
				sb := pixPos / 7        // stream byte index (0–79)
				bp := uint(pixPos % 7)  // bit within that byte
				// The Apple II DHGR hardware forms the colour nibble as:
				//   hw_colour = p3Ã1 + p0Ã2 + p1Ã4 + p2Ã8
				// Colour bit 0 comes from the RIGHTMOST pixel of the cell;
				// colour bit 1 from the leftmost, and so on.
				// Writing colour bit (bit+1)%4 at pixel position `bit`
				// causes the hardware to reconstruct exactly colour c.
				colorBit := (bit + 1) % 4
				if (c>>uint(colorBit))&1 != 0 {
					stream[sb] |= 1 << bp
				}
			}
		}

		// Split: even stream bytes → aux page, odd stream bytes → main page.
		for sb := 0; sb < 80; sb++ {
			if sb%2 == 0 {
				aux[off+sb/2] = stream[sb]
			} else {
				main[off+sb/2] = stream[sb]
			}
		}
	}
	return
}

// ── preview PNG ───────────────────────────────────────────────────────────────

// dhgrToPreview decodes the aux+main pages back into colour cells and renders
// a 560×192 PNG using the true DHGR palette.  Each colour cell is drawn as
// 4 consecutive pixels in the output image.
func dhgrToPreview(aux [dhgrAuxSz]byte, main [dhgrMainSz]byte) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 560, dhgrH))

	for y := 0; y < dhgrH; y++ {
		off := hgrOffset(y)

		// Reconstruct the 80-byte interleaved stream from aux and main pages.
		var stream [80]byte
		for b := 0; b < 40; b++ {
			stream[b*2] = aux[off+b]
			stream[b*2+1] = main[off+b]
		}

		// Decode each colour cell and paint 4 physical pixels.
		for cellX := 0; cellX < dhgrCellW; cellX++ {
			var idx byte
			for bit := 0; bit < 4; bit++ {
				pixPos := cellX*4 + bit
				sb := pixPos / 7
				bp := uint(pixPos % 7)
				if stream[sb]&(1<<bp) != 0 {
					// Mirror the encoder's mapping: pixel position `bit` holds
					// colour bit (bit+1)%4, so reconstruct accordingly.
					idx |= 1 << uint((bit+1)%4)
				}
			}
			c := dhgrPalette[idx]
			for px := 0; px < 4; px++ {
				img.SetNRGBA(cellX*4+px, y, c)
			}
		}
	}
	return img
}

// ── HTML templates ────────────────────────────────────────────────────────────

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DHGR Photo Booth — FujiNet</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Share+Tech+Mono&family=VT323&display=swap" rel="stylesheet">
<style>
:root{
  --green:#39ff14;--green-dim:#1a7a08;--green-bright:#aaff80;
  --bg:#040a02;--bg-panel:#060e04;
  --glow:0 0 8px #39ff14,0 0 20px #39ff1466;
  --glow-strong:0 0 12px #39ff14,0 0 40px #39ff1488,0 0 80px #39ff1433;
  --font-mono:'Share Tech Mono',monospace;--font-display:'VT323',monospace;
}
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{min-height:100vh;background:var(--bg);color:var(--green);
  font-family:var(--font-mono);display:flex;flex-direction:column;
  align-items:center;justify-content:flex-start;padding:2rem 1rem 4rem;
  position:relative;overflow-x:hidden}
body::before{content:'';pointer-events:none;position:fixed;inset:0;
  background:repeating-linear-gradient(to bottom,transparent 0,transparent 3px,
  rgba(0,0,0,.18) 3px,rgba(0,0,0,.18) 4px);z-index:1000}
body::after{content:'';pointer-events:none;position:fixed;inset:0;
  background:radial-gradient(ellipse at center,transparent 55%,rgba(0,0,0,.65) 100%);
  z-index:999}
.terminal-header{width:100%;max-width:640px;text-align:center;
  margin-bottom:2rem;animation:fadeIn .6s ease both}
.logo{font-family:var(--font-display);font-size:clamp(2.8rem,8vw,5rem);
  line-height:1;text-shadow:var(--glow-strong);letter-spacing:.05em}
.logo span{color:var(--green-bright)}
.subtitle{font-size:.8rem;color:var(--green-dim);letter-spacing:.2em;
  text-transform:uppercase;margin-top:.4rem}
.cursor{display:inline-block;width:.55em;height:1em;background:var(--green);
  margin-left:3px;vertical-align:middle;animation:blink 1s step-start infinite}
@keyframes blink{50%{opacity:0}}
.palette{display:flex;height:16px;width:100%;max-width:640px;
  margin-bottom:1.5rem;border:1px solid var(--green-dim);border-radius:2px;
  overflow:hidden;animation:fadeIn .6s .1s ease both}
.palette div{flex:1}
.panel{width:100%;max-width:640px;background:var(--bg-panel);
  border:1px solid var(--green-dim);border-radius:2px;
  box-shadow:var(--glow),inset 0 0 60px rgba(57,255,20,.04);
  padding:2rem 2rem 2.5rem;animation:fadeIn .8s .2s ease both;position:relative}
.panel::before,.panel::after{content:'+';position:absolute;
  font-family:var(--font-display);font-size:1.2rem;color:var(--green-dim);line-height:1}
.panel::before{top:6px;left:10px}.panel::after{bottom:6px;right:10px}
.panel-title{font-family:var(--font-display);font-size:1.5rem;
  letter-spacing:.15em;color:var(--green-bright);text-shadow:var(--glow);
  margin-bottom:1.5rem;border-bottom:1px solid var(--green-dim);padding-bottom:.6rem}
.field-label{display:block;font-size:.75rem;letter-spacing:.18em;
  text-transform:uppercase;color:var(--green-dim);margin-bottom:.8rem}
#fileInput{display:none}
.btn-choose{display:block;width:100%;padding:.85rem 1.5rem;
  background:transparent;border:1px solid var(--green-dim);border-radius:2px;
  color:var(--green);font-family:var(--font-display);font-size:1.4rem;
  letter-spacing:.15em;cursor:pointer;text-align:center;margin-bottom:1.2rem;
  transition:border-color .2s,box-shadow .2s}
.btn-choose:hover{border-color:var(--green);box-shadow:var(--glow)}
#preview{display:none;width:100%;border-radius:2px;
  border:1px solid var(--green-dim);margin-bottom:1.2rem}
.btn-transmit{display:block;width:100%;padding:.85rem 1.5rem;
  background:transparent;border:2px solid var(--green);border-radius:2px;
  color:var(--green-bright);font-family:var(--font-display);font-size:1.6rem;
  letter-spacing:.2em;text-transform:uppercase;cursor:pointer;
  box-shadow:var(--glow);transition:background .15s,color .15s,box-shadow .15s;
  position:relative;overflow:hidden}
.btn-transmit::before{content:'';position:absolute;inset:0;
  background:var(--green);transform:translateX(-101%);transition:transform .2s ease}
.btn-transmit:hover::before{transform:translateX(0)}
.btn-transmit:hover{color:#000;box-shadow:var(--glow-strong)}
.btn-transmit:disabled{border-color:var(--green-dim);color:var(--green-dim);
  cursor:default;box-shadow:none}
.btn-transmit:disabled::before{display:none}
.btn-transmit span{position:relative;z-index:1}
.flash{width:100%;max-width:640px;margin-bottom:1.5rem;
  padding:.75rem 1.25rem;border:1px solid;font-size:.85rem;
  letter-spacing:.1em;animation:fadeIn .4s ease both}
.flash.ok{border-color:var(--green);color:var(--green-bright)}
.flash.err{border-color:#ff4040;color:#ff8080}
.status-bar{width:100%;max-width:640px;margin-top:1.2rem;display:flex;
  flex-wrap:wrap;gap:.6rem 2rem;font-size:.68rem;letter-spacing:.12em;
  color:var(--green-dim);animation:fadeIn 1s .3s ease both}
.indicator{display:inline-block;width:7px;height:7px;border-radius:50%;
  background:var(--green);box-shadow:var(--glow);margin-right:.4em;
  vertical-align:middle;animation:pulse 2.5s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}
.endpoint-hint{width:100%;max-width:640px;margin-top:1.5rem;
  padding:1rem 1.25rem;border:1px solid var(--green-dim);
  font-size:.75rem;color:var(--green-dim);line-height:1.8;
  animation:fadeIn 1s .45s ease both}
.endpoint-hint code{color:var(--green);font-size:.85rem}
.hint-title{font-family:var(--font-display);font-size:1rem;
  color:var(--green-bright);letter-spacing:.12em;margin-bottom:.5rem}
@keyframes fadeIn{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:translateY(0)}}
@media(max-width:480px){.panel{padding:1.25rem 1rem 2rem}}
</style>
</head>
<body>
<header class="terminal-header">
  <div class="logo">FUJI<span>NET</span></div>
  <div class="subtitle">APPLE II DHGR PHOTO BOOTH <span class="cursor"></span></div>
</header>

{{if .Flash}}
<div class="flash {{.FlashClass}}">&#x25B6; {{.Flash}}</div>
{{end}}

<div class="palette">
  <div style="background:#000000" title="0 Black"></div>
  <div style="background:#DD0033" title="1 Magenta"></div>
  <div style="background:#000099" title="2 Dark Blue"></div>
  <div style="background:#AA00FF" title="3 Purple"></div>
  <div style="background:#007722" title="4 Dark Green"></div>
  <div style="background:#555555" title="5 Dark Gray"></div>
  <div style="background:#2222FF" title="6 Medium Blue"></div>
  <div style="background:#66AAFF" title="7 Light Blue"></div>
  <div style="background:#885500" title="8 Brown"></div>
  <div style="background:#FF6600" title="9 Orange"></div>
  <div style="background:#AAAAAA" title="10 Light Gray"></div>
  <div style="background:#FF9988" title="11 Pink"></div>
  <div style="background:#00FF44" title="12 Green"></div>
  <div style="background:#FFFF00" title="13 Yellow"></div>
  <div style="background:#44FFCC" title="14 Aqua"></div>
  <div style="background:#FFFFFF" title="15 White"></div>
</div>

<div class="panel">
  <div class="panel-title">// UPLOAD PHOTO &rarr; DHGR</div>
  <form id="frm" action="/upload" method="post" enctype="multipart/form-data">
    <label class="field-label" for="fileInput">Select or take a photo &mdash; converts to 560&times;192, 16 colours</label>
    <input type="file" id="fileInput" name="photo" accept="image/*" capture="environment">
    <label for="fileInput" class="btn-choose">&#x1F4F7; Choose / Take Photo</label>
    <img id="preview" alt="selected photo">
    <button type="submit" class="btn-transmit" id="sub" disabled>
      <span>&#x25B6; CONVERT TO DHGR &#x25B6;</span>
    </button>
  </form>
</div>

<div class="status-bar">
  <span><span class="indicator"></span>FUJINET CONNECTED</span>
  <span>DHGR 560&times;192 / 16 COLOURS</span>
  <span>OUTPUT: <code>/image</code></span>
</div>

<div class="endpoint-hint">
  <div class="hint-title">// DEVICE ENDPOINTS</div>
  <div><code>POST /upload</code>    &mdash; submit a photo for DHGR conversion</div>
  <div><code>GET  /image</code>     &mdash; 16&thinsp;384-byte binary: aux page (0&ndash;8191) + main page (8192&ndash;16383)</div>
  <div><code>GET  /preview</code>   &mdash; 560&times;192 PNG in true DHGR palette colours</div>
  <div><code>GET  /timestamp</code> &mdash; Unix timestamp of last upload (plain integer; 0 if none)</div>
</div>

<script>
const inp=document.getElementById('fileInput');
const sub=document.getElementById('sub');
const prev=document.getElementById('preview');
inp.addEventListener('change',()=>{
  if(!inp.files[0])return;
  const url=URL.createObjectURL(inp.files[0]);
  prev.onload=()=>URL.revokeObjectURL(url);
  prev.src=url;
  prev.style.display='block';
  sub.disabled=false;
});
document.getElementById('frm').addEventListener('submit',()=>{
  sub.disabled=true;
  sub.querySelector('span').textContent='⏳ CONVERTING…';
});
</script>
</body>
</html>`

const successHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DHGR Converted! — FujiNet</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Share+Tech+Mono&family=VT323&display=swap" rel="stylesheet">
<style>
:root{
  --green:#39ff14;--green-dim:#1a7a08;--green-bright:#aaff80;
  --bg:#040a02;--bg-panel:#060e04;
  --glow:0 0 8px #39ff14,0 0 20px #39ff1466;
  --glow-strong:0 0 12px #39ff14,0 0 40px #39ff1488,0 0 80px #39ff1433;
  --font-mono:'Share Tech Mono',monospace;--font-display:'VT323',monospace;
}
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{min-height:100vh;background:var(--bg);color:var(--green);
  font-family:var(--font-mono);display:flex;flex-direction:column;
  align-items:center;justify-content:flex-start;padding:2rem 1rem 4rem;
  position:relative;overflow-x:hidden}
body::before{content:'';pointer-events:none;position:fixed;inset:0;
  background:repeating-linear-gradient(to bottom,transparent 0,transparent 3px,
  rgba(0,0,0,.18) 3px,rgba(0,0,0,.18) 4px);z-index:1000}
body::after{content:'';pointer-events:none;position:fixed;inset:0;
  background:radial-gradient(ellipse at center,transparent 55%,rgba(0,0,0,.65) 100%);
  z-index:999}
.terminal-header{width:100%;max-width:640px;text-align:center;
  margin-bottom:2.5rem;animation:fadeIn .6s ease both}
.logo{font-family:var(--font-display);font-size:clamp(2.8rem,8vw,5rem);
  line-height:1;text-shadow:var(--glow-strong);letter-spacing:.05em}
.logo span{color:var(--green-bright)}
.subtitle{font-size:.8rem;color:var(--green-dim);letter-spacing:.2em;
  text-transform:uppercase;margin-top:.4rem}
.panel{width:100%;max-width:640px;background:var(--bg-panel);
  border:1px solid var(--green-dim);border-radius:2px;
  box-shadow:var(--glow),inset 0 0 60px rgba(57,255,20,.04);
  padding:2rem 2rem 2.5rem;animation:fadeIn .8s .15s ease both;position:relative}
.panel::before,.panel::after{content:'+';position:absolute;
  font-family:var(--font-display);font-size:1.2rem;color:var(--green-dim);line-height:1}
.panel::before{top:6px;left:10px}.panel::after{bottom:6px;right:10px}
.panel-title{font-family:var(--font-display);font-size:1.5rem;
  letter-spacing:.15em;color:var(--green-bright);text-shadow:var(--glow);
  margin-bottom:1.2rem;border-bottom:1px solid var(--green-dim);padding-bottom:.6rem}
.preview-img{width:100%;border-radius:2px;border:1px solid var(--green-dim);
  margin-bottom:1.2rem;image-rendering:pixelated;display:block}
.meta{font-size:.72rem;color:var(--green-dim);line-height:2;margin-bottom:1.4rem}
.meta code{color:var(--green)}
.actions{display:flex;gap:.75rem;flex-wrap:wrap}
.btn{padding:.85rem 1.25rem;border-radius:2px;font-family:var(--font-display);
  font-size:1.3rem;letter-spacing:.12em;text-decoration:none;cursor:pointer;
  border:none;text-align:center;flex:1;min-width:140px;display:block}
.btn-again{background:var(--green);color:#000;font-weight:bold;box-shadow:var(--glow)}
.btn-dl{background:transparent;color:var(--green);
  border:1px solid var(--green-dim);font-size:1.1rem}
.btn-dl:hover{border-color:var(--green)}
@keyframes fadeIn{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:translateY(0)}}
@media(max-width:480px){.panel{padding:1.25rem 1rem 2rem}}
</style>
</head>
<body>
<header class="terminal-header">
  <div class="logo">FUJI<span>NET</span></div>
  <div class="subtitle">CONVERSION COMPLETE</div>
</header>
<div class="panel">
  <div class="panel-title">&#x2714; DHGR IMAGE READY</div>
  <img class="preview-img" src="/preview" alt="DHGR preview — 560x192, 16 colours">
  <div class="meta">
    <div>RESOLUTION &nbsp;<code>560 &times; 192 px  &mdash;  140 colour cells &times; 192 rows</code></div>
    <div>COLOURS &nbsp;&nbsp;&nbsp;&nbsp;<code>16  (4-BIT DHGR PALETTE)</code></div>
    <div>DITHER &nbsp;&nbsp;&nbsp;&nbsp;<code>FLOYD-STEINBERG  (RGB SPACE)</code></div>
    <div>AUX PAGE &nbsp;&nbsp;<code>BYTES 0&ndash;8191   &mdash;  BLOAD $2000  (AUX)</code></div>
    <div>MAIN PAGE &nbsp;<code>BYTES 8192&ndash;16383  &mdash;  BLOAD $2000  (MAIN)</code></div>
    <div>TOTAL SIZE &nbsp;<code>16 384 BYTES</code></div>
  </div>
  <div class="actions">
    <a href="/" class="btn btn-again">&#x21BA; UPLOAD ANOTHER</a>
    <a href="/image" class="btn btn-dl" download="screen.dhgr">&#x2B07; DOWNLOAD .DHGR</a>
  </div>
</div>
</body>
</html>`

// ── Template data ─────────────────────────────────────────────────────────────

type pageData struct {
	Flash      string
	FlashClass string
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	flash := r.URL.Query().Get("flash")
	fc := r.URL.Query().Get("fc")
	tmpl := template.Must(template.New("index").Parse(indexHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, pageData{Flash: flash, FlashClass: fc}); err != nil {
		log.Printf("template error: %v", err)
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Redirect(w, r, "/?flash=ERROR+PARSING+UPLOAD&fc=err", http.StatusSeeOther)
		return
	}

	file, hdr, err := r.FormFile("photo")
	if err != nil {
		http.Redirect(w, r, "/?flash=NO+PHOTO+FIELD+IN+FORM&fc=err", http.StatusSeeOther)
		return
	}
	defer file.Close()

	// Read the entire upload into memory so we can (a) parse EXIF before
	// the image decoder consumes the reader, and (b) decode the image.
	rawBytes, err := io.ReadAll(file)
	if err != nil {
		http.Redirect(w, r, "/?flash=CANNOT+READ+UPLOAD&fc=err", http.StatusSeeOther)
		return
	}

	// Extract EXIF orientation before decoding (decoder discards it).
	orient := exifOrientation(rawBytes)
	log.Printf("upload  file=%q  src=%s  exif_orientation=%d", hdr.Filename, r.RemoteAddr, orient)

	img, format, err := image.Decode(bytes.NewReader(rawBytes))
	if err != nil {
		http.Redirect(w, r, "/?flash=CANNOT+DECODE+IMAGE&fc=err", http.StatusSeeOther)
		return
	}

	// Apply EXIF orientation so portrait shots appear upright on the Apple II.
	img = applyOrientation(img, orient)
	log.Printf("decoded format=%s  bounds=%v  (after orientation correction)", format, img.Bounds())

	aux, main := convertToDHGR(img)

	mu.Lock()
	auxPage = aux
	mainPage = main
	lastUpload = time.Now()
	hasImage = true
	mu.Unlock()

	log.Printf("DHGR conversion complete at %s", lastUpload.Format(time.RFC3339))

	// Serve success page directly so /preview is always fresh.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(successHTML))
}

// handleImage serves the full 16 384-byte DHGR binary:
// auxiliary page (bytes 0–8191) followed by main page (bytes 8192–16383).
func handleImage(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	ok, aux, main := hasImage, auxPage, mainPage
	mu.RUnlock()

	if !ok {
		http.Error(w, "NO IMAGE AVAILABLE YET", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="screen.dhgr"`)
	w.Header().Set("Content-Length", "16384")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(aux[:])
	w.Write(main[:])
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	ok, aux, main := hasImage, auxPage, mainPage
	mu.RUnlock()

	if !ok {
		http.Error(w, "NO IMAGE AVAILABLE YET", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := png.Encode(w, dhgrToPreview(aux, main)); err != nil {
		log.Printf("preview encode error: %v", err)
	}
}

func handleTimestamp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	mu.RLock()
	ok, ts := hasImage, lastUpload
	mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !ok {
		fmt.Fprint(w, "0")
		return
	}
	fmt.Fprintf(w, "%d", ts.Unix())
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/image", handleImage)
	http.HandleFunc("/preview", handlePreview)
	http.HandleFunc("/timestamp", handleTimestamp)

	addr := ":8080"
	log.Println("┌────────────────────────────────────────────────────────────────")
	log.Println("│  Apple II DHGR Photo Booth — VCF / FujiNet Booth")
	log.Println("├────────────────────────────────────────────────────────────────")
	log.Printf("│  Upload form  →  http://HOST%s/", addr)
	log.Printf("│  Raw DHGR     →  http://HOST%s/image      (16 384 B: aux+main)", addr)
	log.Printf("│  PNG preview  →  http://HOST%s/preview    (560×192, 16 colours)", addr)
	log.Printf("│  Timestamp    →  http://HOST%s/timestamp  (Unix int, plain text)", addr)
	log.Println("└────────────────────────────────────────────────────────────────")
	log.Fatal(http.ListenAndServe(addr, nil))
}
