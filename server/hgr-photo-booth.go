// hgr-photo-booth — VCF FujiNet Booth Tool
//
// Accepts a photo upload from a phone, converts it to Apple II HGR format
// (280×192, monochrome, Floyd-Steinberg dithered), and stores the raw
// 8 192-byte HGR page for retrieval by FujiNet or any HTTP client.
//
// Endpoints
//
//	GET  /           – mobile-friendly HTML upload form (CRT aesthetic)
//	POST /upload     – multipart image upload; shows success page with preview
//	GET  /image      – raw 8 192-byte HGR binary (BLOAD at $2000)
//	GET  /preview    – 2× PNG (560×384) rendered in Apple II phosphor green
//	GET  /timestamp  – Unix timestamp of last upload as plain text (integer)
//
// Stdlib only — no external dependencies.

package main

import (
	"fmt"
	"html/template"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"log"
	"net/http"
	"sync"
	"time"
)

// ── HGR constants ─────────────────────────────────────────────────────────────

const (
	hgrW    = 280  // pixels per row  (40 bytes × 7 bits)
	hgrH    = 192  // total scanlines
	hgrRow  = 40   // bytes per scanline
	hgrPage = 8192 // full HGR page size
)

// hgrOffset returns the byte offset within a linear 8 192-byte HGR page for
// scanline y.  Apple II HGR memory is non-linear:
//
//	offset = (y % 8)        × 0x0400
//	       + ((y / 8) % 8)  × 0x0080
//	       + (y / 64)       × 0x0028
func hgrOffset(y int) int {
	return (y&7)*0x0400 + ((y>>3)&7)*0x0080 + (y>>6)*0x0028
}

// ── shared state ──────────────────────────────────────────────────────────────

var (
	mu         sync.RWMutex
	hgrData    [hgrPage]byte
	lastUpload time.Time
	hasImage   bool
)

// ── image conversion ──────────────────────────────────────────────────────────

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

			// RGBA() returns 16-bit (0-65535) pre-multiplied values.
			r00, g00, b00, _ := src.At(ox+x0, oy+y0).RGBA()
			r10, g10, b10, _ := src.At(ox+x1, oy+y0).RGBA()
			r01, g01, b01, _ := src.At(ox+x0, oy+y1).RGBA()
			r11, g11, b11, _ := src.At(ox+x1, oy+y1).RGBA()

			lerpF := func(a, b float64, t float64) float64 {
				return a*(1-t) + b*t
			}
			r := lerpF(lerpF(float64(r00), float64(r10), fx), lerpF(float64(r01), float64(r11), fx), fy) / 257
			g := lerpF(lerpF(float64(g00), float64(g10), fx), lerpF(float64(g01), float64(g11), fx), fy) / 257
			b := lerpF(lerpF(float64(b00), float64(b10), fx), lerpF(float64(b01), float64(b11), fx), fy) / 257

			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff,
			})
		}
	}
	return dst
}

// convertToHGR scales src to 280×192, applies Floyd-Steinberg dithering, and
// packs the result into the Apple II HGR interleaved memory layout.
// The returned 8 192-byte array is ready to BLOAD at $2000.
func convertToHGR(src image.Image) [hgrPage]byte {
	// 1. Scale to 280×192 with bilinear interpolation.
	scaled := bilinearScale(src, hgrW, hgrH)

	// 2. Extract ITU-R BT.601 luminance into a float64 grid.
	lum := make([]float64, hgrW*hgrH)
	for y := 0; y < hgrH; y++ {
		for x := 0; x < hgrW; x++ {
			c := scaled.NRGBAAt(x, y)
			lum[y*hgrW+x] = 0.299*float64(c.R) +
				0.587*float64(c.G) +
				0.114*float64(c.B)
		}
	}

	// 3. Floyd-Steinberg dithering → 1-bit pixel grid.
	//
	//         *   7/16
	//   3/16  5/16  1/16
	pixels := make([]bool, hgrW*hgrH)
	for y := 0; y < hgrH; y++ {
		for x := 0; x < hgrW; x++ {
			i := y*hgrW + x
			old := lum[i]
			var newV float64
			if old >= 128 {
				newV = 255
				pixels[i] = true
			}
			e := old - newV
			if x+1 < hgrW {
				lum[i+1] += e * 7 / 16
			}
			if y+1 < hgrH {
				if x > 0 {
					lum[i+hgrW-1] += e * 3 / 16
				}
				lum[i+hgrW] += e * 5 / 16
				if x+1 < hgrW {
					lum[i+hgrW+1] += e * 1 / 16
				}
			}
		}
	}

	// 4. Pack 1-bit pixels into the HGR interleaved page.
	//
	// Each byte stores 7 pixel bits (bits 0-6); bit 7 is the palette-select
	// flag (0 = palette A).  Pixel x lives in byte floor(x/7), bit x%7.
	var page [hgrPage]byte
	for y := 0; y < hgrH; y++ {
		off := hgrOffset(y)
		for b := 0; b < hgrRow; b++ {
			var byt byte
			for bit := 0; bit < 7; bit++ {
				x := b*7 + bit
				if x < hgrW && pixels[y*hgrW+x] {
					byt |= 1 << uint(bit)
				}
			}
			page[off+b] = byt
		}
	}
	return page
}

// hgrToPNG renders the HGR page as a 2× PNG (560×384) in Apple II
// phosphor green (#5FFF5F) on black.
func hgrToPNG(data [hgrPage]byte) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, hgrW*2, hgrH*2))
	phosphor := color.NRGBA{R: 0x5f, G: 0xff, B: 0x5f, A: 0xff}
	black := color.NRGBA{A: 0xff}
	for y := 0; y < hgrH; y++ {
		off := hgrOffset(y)
		for b := 0; b < hgrRow; b++ {
			byt := data[off+b]
			for bit := 0; bit < 7; bit++ {
				x := b*7 + bit
				c := black
				if byt&(1<<uint(bit)) != 0 {
					c = phosphor
				}
				img.SetNRGBA(x*2, y*2, c)
				img.SetNRGBA(x*2+1, y*2, c)
				img.SetNRGBA(x*2, y*2+1, c)
				img.SetNRGBA(x*2+1, y*2+1, c)
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
<title>HGR Photo Booth — FujiNet</title>
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
.cursor{display:inline-block;width:.55em;height:1em;background:var(--green);
  margin-left:3px;vertical-align:middle;animation:blink 1s step-start infinite}
@keyframes blink{50%{opacity:0}}
.panel{width:100%;max-width:640px;background:var(--bg-panel);
  border:1px solid var(--green-dim);border-radius:2px;
  box-shadow:var(--glow),inset 0 0 60px rgba(57,255,20,.04);
  padding:2rem 2rem 2.5rem;animation:fadeIn .8s .15s ease both;position:relative}
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
  border:1px solid var(--green-dim);margin-bottom:1.2rem;
  image-rendering:pixelated}
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
  <div class="subtitle">APPLE II HGR PHOTO BOOTH <span class="cursor"></span></div>
</header>

{{if .Flash}}
<div class="flash {{.FlashClass}}">&#x25B6; {{.Flash}}</div>
{{end}}

<div class="panel">
  <div class="panel-title">// UPLOAD PHOTO &rarr; HGR</div>
  <form id="frm" action="/upload" method="post" enctype="multipart/form-data">
    <label class="field-label" for="fileInput">Select or take a photo to convert to 280&times;192 HGR</label>
    <input type="file" id="fileInput" name="photo" accept="image/*" capture="environment">
    <label for="fileInput" class="btn-choose">&#x1F4F7; Choose / Take Photo</label>
    <img id="preview" alt="selected photo">
    <button type="submit" class="btn-transmit" id="sub" disabled>
      <span>&#x25B6; CONVERT TO HGR &#x25B6;</span>
    </button>
  </form>
</div>

<div class="status-bar">
  <span><span class="indicator"></span>FUJINET CONNECTED</span>
  <span>HGR: 280&times;192 MONO</span>
  <span>OUTPUT: <code>/image</code></span>
</div>

<div class="endpoint-hint">
  <div class="hint-title">// DEVICE ENDPOINTS</div>
  <div><code>POST /upload</code>    &mdash; submit a photo for HGR conversion</div>
  <div><code>GET  /image</code>     &mdash; raw 8&thinsp;192-byte HGR page (BLOAD at $2000)</div>
  <div><code>GET  /preview</code>   &mdash; 560&times;384 PNG in Apple II phosphor green</div>
  <div><code>GET  /timestamp</code> &mdash; Unix timestamp of last upload (plain integer)</div>
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
<title>HGR Converted! — FujiNet</title>
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
  <div class="panel-title">&#x2714; HGR IMAGE READY</div>
  <img class="preview-img" src="/preview" alt="HGR preview — Apple II phosphor green">
  <div class="meta">
    <div>RESOLUTION &nbsp;<code>280 &times; 192 px</code></div>
    <div>COLOUR &nbsp;&nbsp;&nbsp;&nbsp;<code>MONOCHROME (1-BIT)</code></div>
    <div>DITHER &nbsp;&nbsp;&nbsp;&nbsp;<code>FLOYD-STEINBERG</code></div>
    <div>PAGE SIZE &nbsp;<code>8 192 BYTES</code></div>
    <div>BLOAD AT &nbsp;&nbsp;<code>$2000</code></div>
  </div>
  <div class="actions">
    <a href="/" class="btn btn-again">&#x21BA; UPLOAD ANOTHER</a>
    <a href="/image" class="btn btn-dl" download="screen.hgr">&#x2B07; DOWNLOAD .HGR</a>
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

	// Cap body at 20 MB.
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

	img, format, err := image.Decode(file)
	if err != nil {
		http.Redirect(w, r, "/?flash=CANNOT+DECODE+IMAGE&fc=err", http.StatusSeeOther)
		return
	}
	log.Printf("upload  file=%q  format=%s  src=%s  bounds=%v",
		hdr.Filename, format, r.RemoteAddr, img.Bounds())

	page := convertToHGR(img)

	mu.Lock()
	hgrData = page
	lastUpload = time.Now()
	hasImage = true
	mu.Unlock()

	log.Printf("HGR conversion complete at %s", lastUpload.Format(time.RFC3339))

	// Serve success page directly so /preview is always fresh (no redirect).
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(successHTML))
}

func handleImage(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	ok, data := hasImage, hgrData
	mu.RUnlock()

	if !ok {
		http.Error(w, "NO IMAGE AVAILABLE YET", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="screen.hgr"`)
	w.Header().Set("Content-Length", "8192")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data[:])
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	ok, data := hasImage, hgrData
	mu.RUnlock()

	if !ok {
		http.Error(w, "NO IMAGE AVAILABLE YET", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := png.Encode(w, hgrToPNG(data)); err != nil {
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
	log.Println("┌──────────────────────────────────────────────────")
	log.Println("│  Apple II HGR Photo Booth — VCF / FujiNet Booth")
	log.Println("├──────────────────────────────────────────────────")
	log.Printf("│  Upload form  →  http://HOST%s/", addr)
	log.Printf("│  Raw HGR      →  http://HOST%s/image      (8192 B, BLOAD $2000)", addr)
	log.Printf("│  PNG preview  →  http://HOST%s/preview    (560×384, phosphor green)", addr)
	log.Printf("│  Timestamp    →  http://HOST%s/timestamp  (Unix int, plain text)", addr)
	log.Println("└──────────────────────────────────────────────────")
	log.Fatal(http.ListenAndServe(addr, nil))
}
