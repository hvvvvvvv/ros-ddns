package api

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; frame-ancestors 'none'; base-uri 'none'"

func registerUI(mux *http.ServeMux, files fs.FS) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveUIFile(w, r, files, "index.html", "no-store")
	})
	mux.HandleFunc("GET /assets/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serveUIFile(w, r, files, r.PathValue("path"), "no-cache")
	})
}

func serveUIFile(w http.ResponseWriter, r *http.Request, files fs.FS, name, cache string) {
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if files == nil || !fs.ValidPath(name) || name == "." || strings.Contains(name, `\`) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	file, err := files.Open(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read UI file")
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(content))
}
