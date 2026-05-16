package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func compile(orchestrator, srcPath, outDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	req, err := http.NewRequest(http.MethodPost, orchestrator+"/compile", f)
	if err != nil {
		return err
	}
	req.Header.Set("X-Filename", filepath.Base(srcPath))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", srcPath, body)
	}

	objName := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath)) + ".o"
	return os.WriteFile(filepath.Join(outDir, objName), body, 0644)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: client <file.c> [file.c ...]")
		os.Exit(1)
	}
	sources := os.Args[1:]

	orchestrator := os.Getenv("ORCHESTRATOR_ADDR")
	if orchestrator == "" {
		orchestrator = "http://localhost:8080"
	}
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = "."
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(sources))

	for _, src := range sources {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			if err := compile(orchestrator, src, outDir); err != nil {
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
