package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Flags whose value is a separate argument, so both tokens must stay together.
var flagTakesValue = map[string]bool{
	"-I": true, "-D": true, "-U": true, "-include": true, "-imacros": true,
	"-isystem": true, "-iquote": true, "-idirafter": true, "-x": true,
}

// The preprocessed translation unit is ~20x the source size and is almost all
// header text, so it compresses ~10x. One transport is shared by every request:
// net/http's default caps idle connections per host at 2, which makes a fan-out
// of hundreds of compiles reconnect for nearly every file.
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
	},
}

// splitArgs separates compiler flags from source files so flags can be applied
// to both the local preprocess and the remote compile.
func splitArgs(args []string) (flags, sources []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			sources = append(sources, arg)
			continue
		}
		flags = append(flags, arg)
		if flagTakesValue[arg] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, sources
}

// preprocess expands includes and macros locally so the worker receives a
// self-contained translation unit; it has no access to the client's headers.
// This step is unavoidably local, so it sets the ceiling on any speedup:
// distribution can only ever remove the codegen half of each compile.
func preprocess(cc string, flags []string, srcPath string) ([]byte, error) {
	args := append([]string{"-E"}, flags...)
	cmd := exec.Command(cc, append(args, srcPath)...)
	var stdout, stderr bytes.Buffer
	// Preprocessed output is large; grow once instead of reallocating up the
	// doubling ladder for every file.
	stdout.Grow(1 << 20)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("preprocess failed: %w\n%s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func compile(cc, orchestrator string, flags []string, srcPath, outDir string, gate chan struct{}) error {
	// Preprocessing forks a compiler, so it is bounded by local cores. The
	// remote request that follows is pure wait and must not hold the slot.
	gate <- struct{}{}
	preprocessed, err := preprocess(cc, flags, srcPath)
	<-gate
	if err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}

	base := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))

	var body bytes.Buffer
	body.Grow(len(preprocessed) / 8)
	zw, _ := gzip.NewWriterLevel(&body, gzip.BestSpeed)
	if _, err := zw.Write(preprocessed); err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}

	req, err := http.NewRequest(http.MethodPost, orchestrator+"/compile", bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.ContentLength = int64(body.Len())
	req.Header.Set("X-Filename", base+".i")
	req.Header.Set("Content-Encoding", "gzip")
	if len(flags) > 0 {
		encoded, err := json.Marshal(flags)
		if err != nil {
			return err
		}
		req.Header.Set("X-Compile-Flags", string(encoded))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	obj, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", srcPath, obj)
	}

	return os.WriteFile(filepath.Join(outDir, base+".o"), obj, 0644)
}

func main() {
	flags, sources := splitArgs(os.Args[1:])
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "usage: client [compiler flags] <file.c> [file.c ...]")
		os.Exit(1)
	}

	orchestrator := os.Getenv("ORCHESTRATOR_ADDR")
	if orchestrator == "" {
		orchestrator = "http://localhost:8080"
	}
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = "."
	}
	// DISTBUILD_CC takes precedence: under `make CC=distbuild_wrapper.sh`, CC
	// names the wrapper itself, which would recurse instead of preprocessing.
	cc := os.Getenv("DISTBUILD_CC")
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		cc = "gcc"
	}

	// Local preprocessing is CPU-bound; running one per source file at once
	// would fork hundreds of compilers and thrash.
	// GOMAXPROCS respects the cgroup CPU quota; NumCPU would report the whole
	// host even when the client is capped with --cpus.
	parallel := runtime.GOMAXPROCS(0)
	if v := os.Getenv("DISTBUILD_PREPROCESS_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			parallel = n
		}
	}
	gate := make(chan struct{}, parallel)

	var wg sync.WaitGroup
	errs := make(chan error, len(sources))

	for _, src := range sources {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			if err := compile(cc, orchestrator, flags, src, outDir, gate); err != nil {
				errs <- err
			}
		}(src)
	}
	wg.Wait()
	close(errs)

	failed := false
	for err := range errs {
		fmt.Fprintln(os.Stderr, err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	fmt.Printf("compiled %d file(s) to %s\n", len(sources), outDir)
}
