package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/disintegration/imaging"
)

//go:embed static
var staticFiles embed.FS

type appState struct {
	mu         sync.RWMutex
	uploadedAt time.Time
	v9938Data  []byte // palette (32 bytes) + bitmap (54272 bytes) = 54304 bytes total
	previewPNG []byte
	hasImage   bool
}

var state appState

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "missing image field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read error: "+err.Error(), http.StatusBadRequest)
		return
	}

	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		http.Error(w, "image decode error: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	v9938Data, preview := ConvertToV9938(img)

	var buf bytes.Buffer
	if err := png.Encode(&buf, preview); err != nil {
		http.Error(w, "preview encode error", http.StatusInternalServerError)
		return
	}

	state.mu.Lock()
	state.uploadedAt = time.Now()
	state.v9938Data = v9938Data
	state.previewPNG = buf.Bytes()
	state.hasImage = true
	state.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintln(w, "ok")
}

func handleTimestamp(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	ts := state.uploadedAt
	has := state.hasImage
	state.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !has {
		fmt.Fprintln(w, "0")
		return
	}
	fmt.Fprintln(w, ts.Unix())
}

// get1Size: palette (32 B) + first 106 rows of bitmap (106 × 256 = 27136 B) = 27168 B
const get1Size = 32 + 106*256

func handleGet(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	data := state.v9938Data
	has := state.hasImage
	state.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !has {
		http.Error(w, "no image available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	// Binary layout:
	//     0–31   Palette (16 × 2 bytes, V9938 register format)
	//   32–54303 Bitmap  (512×212, 4bpp, 2 pixels per byte)
	w.Write(data)
}

// handleGet1 returns the first half of the V9938 blob: palette (32 B) + bitmap
// rows 0-105 (27136 B) = 27168 B total.  Kept small so FujiNet's internal HTTP
// receive buffer is never exhausted in a single connection.
func handleGet1(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	data := state.v9938Data
	has := state.hasImage
	state.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !has {
		http.Error(w, "no image available", http.StatusNotFound)
		return
	}

	end := get1Size
	if end > len(data) {
		end = len(data)
	}
	slice := data[:end]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(slice)))
	w.Write(slice)
}

// handleGet2 returns the second half of the V9938 blob: bitmap rows 106-211
// (27136 B).  Fetched by the client in a second connection after /get1.
func handleGet2(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	data := state.v9938Data
	has := state.hasImage
	state.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !has {
		http.Error(w, "no image available", http.StatusNotFound)
		return
	}

	if get1Size >= len(data) {
		http.Error(w, "no second half available", http.StatusNotFound)
		return
	}
	slice := data[get1Size:]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(slice)))
	w.Write(slice)
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	data := state.previewPNG
	has := state.hasImage
	state.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !has {
		http.Error(w, "no image available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	subFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(subFS)))

	mux.HandleFunc("/upload", handleUpload)
	mux.HandleFunc("/timestamp", handleTimestamp)
	mux.HandleFunc("/get", handleGet)
	mux.HandleFunc("/get1", handleGet1)
	mux.HandleFunc("/get2", handleGet2)
	mux.HandleFunc("/preview", handlePreview)

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}
	srv.SetKeepAlivesEnabled(false)

	log.Printf("V9938 Mode 7 Photo Booth listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
