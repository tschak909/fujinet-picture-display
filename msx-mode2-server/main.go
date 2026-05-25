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
	tmsData    []byte // nameTable + patternTable + colorTable (13056 bytes)
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

	// Buffer the upload so imaging can read EXIF before decoding pixels.
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// AutoOrientation reads the EXIF orientation tag (if present) and rotates
	// the image into upright position before we do anything else.
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		http.Error(w, "image decode error: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	nameTable, patternTable, colorTable, preview := ConvertToTMS9918(img)

	var buf bytes.Buffer
	if err := png.Encode(&buf, preview); err != nil {
		http.Error(w, "preview encode error", http.StatusInternalServerError)
		return
	}

	// Concatenate tables: name (768) + pattern (6144) + color (6144) = 13056 bytes
	tmsData := make([]byte, 0, 768+6144+6144)
	tmsData = append(tmsData, nameTable...)
	tmsData = append(tmsData, patternTable...)
	tmsData = append(tmsData, colorTable...)

	state.mu.Lock()
	state.uploadedAt = time.Now()
	state.tmsData = tmsData
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

func handleGet(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	data := state.tmsData
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
	//   0–767    Pattern Name Table  (768 bytes, sequential 0x00–0xFF × 3)
	//   768–6911 Pattern Generator Table (6144 bytes)
	//   6912–13055 Color Table (6144 bytes)
	w.Write(data)
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

	// Strip the "static/" prefix from the embedded FS so "/" serves index.html
	subFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(subFS)))

	mux.HandleFunc("/upload", handleUpload)
	mux.HandleFunc("/timestamp", handleTimestamp)
	mux.HandleFunc("/get", handleGet)
	mux.HandleFunc("/preview", handlePreview)

	log.Printf("TMS9918 Photo Booth listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
