package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func compileHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.Header.Get("X-Filename")
	if filename == "" {
		http.Error(w, "missing X-Filename header", http.StatusBadRequest)
		return
	}

	dir, err := os.MkdirTemp("", "distbuild-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, filename)
	src, err := os.Create(srcPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(src, r.Body)
	src.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	objPath := strings.TrimSuffix(srcPath, filepath.Ext(srcPath)) + ".o"
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "gcc"
	}

	out, err := exec.Command(cc, "-c", srcPath, "-o", objPath).CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("compile failed: %s\n%s", err, out), http.StatusUnprocessableEntity)
		return
	}

	obj, err := os.Open(objPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer obj.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, obj)
}

func main() {
	addr := os.Getenv("WORKER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	http.HandleFunc("/compile", compileHandler)
	log.Println("worker listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
