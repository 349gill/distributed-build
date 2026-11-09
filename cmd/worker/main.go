package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The worker decides where the object goes, and the input arrives already
// preprocessed, so output and preprocessor flags are dropped; codegen flags
// (-O2, -g, -std=, -W...) must be kept or the object would differ from a local build.
var droppedFlags = map[string]bool{
	"-o": true, "-MF": true, "-MT": true, "-MQ": true,
	"-I": true, "-D": true, "-U": true, "-include": true, "-imacros": true,
	"-isystem": true, "-iquote": true, "-idirafter": true,
}

var droppedStandaloneFlags = map[string]bool{"-c": true, "-E": true, "-M": true, "-MM": true, "-MD": true, "-MMD": true}

// Each compile forks a compiler, so accepting more of them than the machine has
// cores only adds context switching; the rest queue on this token instead.
var slots chan struct{}

func isSafeParam(val string) bool {
	if val == "" {
		return false
	}
	for _, r := range val {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '+' || r == '=' || r == '.') {
			return false
		}
	}
	return true
}

func isSafeFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}

	switch arg {
	case "-W", "-O", "-O0", "-O1", "-O2", "-O3", "-Os", "-Ofast", "-Og", "-Oz",
		"-g", "-g0", "-g1", "-g2", "-g3", "-ggdb",
		"-pipe", "-pthreads", "-pthread",
		"-fPIC", "-fpic", "-fPIE", "-fpie",
		"-fno-strict-aliasing", "-ffast-math",
		"-fstack-protector", "-fstack-protector-strong", "-fstack-protector-all", "-fno-stack-protector",
		"-fomit-frame-pointer", "-fno-omit-frame-pointer",
		"-fexceptions", "-fno-exceptions",
		"-frtti", "-fno-rtti",
		"-fdata-sections", "-ffunction-sections",
		"-flto", "-fno-lto",
		"-funsigned-char", "-fsigned-char",
		"-fwrapv", "-fno-wrapv",
		"-fcommon", "-fno-common",
		"-fmath-errno", "-fno-math-errno",
		"-m32", "-m64":
		return true
	}

	if strings.HasPrefix(arg, "-std=") {
		return isSafeParam(arg[len("-std="):])
	}
	if strings.HasPrefix(arg, "-march=") {
		return isSafeParam(arg[len("-march="):])
	}
	if strings.HasPrefix(arg, "-mtune=") {
		return isSafeParam(arg[len("-mtune="):])
	}
	if strings.HasPrefix(arg, "-mcpu=") {
		return isSafeParam(arg[len("-mcpu="):])
	}
	if strings.HasPrefix(arg, "-fvisibility=") {
		return isSafeParam(arg[len("-fvisibility="):])
	}
	if strings.HasPrefix(arg, "-m") && !strings.Contains(arg, "=") {
		return isSafeParam(arg[len("-m"):])
	}
	if strings.HasPrefix(arg, "-W") {
		if strings.HasPrefix(arg, "-Wl,") || strings.HasPrefix(arg, "-Wp,") || strings.HasPrefix(arg, "-Wa,") {
			return false
		}
		return isSafeParam(arg[len("-W"):])
	}

	return false
}

func parseFlags(header string) ([]string, error) {
	if header == "" {
		return nil, nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(header), &raw); err != nil {
		return nil, err
	}

	var flags []string
	for i := 0; i < len(raw); i++ {
		arg := raw[i]
		if droppedStandaloneFlags[arg] {
			continue
		}
		if droppedFlags[arg] {
			i++ // also drop the flag's value
			continue
		}
		if strings.HasPrefix(arg, "-I") || strings.HasPrefix(arg, "-D") || strings.HasPrefix(arg, "-U") {
			continue // joined form, e.g. -DFOO=1
		}
		if !isSafeFlag(arg) {
			return nil, fmt.Errorf("disallowed or unsafe compile flag: %s", arg)
		}
		flags = append(flags, arg)
	}
	return flags, nil
}

func compileHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.Header.Get("X-Filename")
	if filename == "" {
		http.Error(w, "missing X-Filename header", http.StatusBadRequest)
		return
	}

	// The client gzips the translation unit; Go does not decode a request body
	// automatically the way it does a response.
	body := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer zr.Close()
		body = zr
	}

	dir, err := os.MkdirTemp("", "distbuild-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)

	// filename is attacker-controlled; Base strips any path traversal.
	srcPath := filepath.Join(dir, filepath.Base(filename))
	src, err := os.Create(srcPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(src, body)
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

	flags, err := parseFlags(r.Header.Get("X-Compile-Flags"))
	if err != nil {
		http.Error(w, "invalid X-Compile-Flags: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The .i extension already tells the compiler the input is preprocessed, so
	// it skips cpp on its own; -fpreprocessed would be redundant and is gcc-only.
	args := append(flags, "-c", srcPath, "-o", objPath)

	slots <- struct{}{}
	out, err := exec.CommandContext(r.Context(), cc, args...).CombinedOutput()
	<-slots
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

	// GOMAXPROCS, unlike NumCPU, accounts for the cgroup CPU quota, so a worker
	// started with `docker run --cpus 0.8` sizes itself to its share instead of
	// to the whole host. Ten workers on an 8-CPU host would otherwise each admit
	// 8 compiles and fork 80 compilers onto 8 cores.
	parallel := runtime.GOMAXPROCS(0)
	if v := os.Getenv("WORKER_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			parallel = n
		}
	}
	slots = make(chan struct{}, parallel)

	http.HandleFunc("/compile", compileHandler)
	log.Println("worker listening on", addr, "with", parallel, "compile slot(s)")
	log.Fatal(http.ListenAndServe(addr, nil))
}