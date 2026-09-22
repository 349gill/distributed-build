package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Workers are found by resolving a headless service name to one IP per pod.
// Resolving on every compile put a DNS round trip in front of each file, so the
// set is cached and refreshed in the background instead.
type workerPool struct {
	service string
	port    string
	ttl     time.Duration

	mu        sync.RWMutex
	addrs     []string
	refreshed time.Time

	next uint64
}

func (p *workerPool) resolve() ([]string, error) {
	ips, err := net.LookupHost(p.service)
	if err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip, p.port))
	}
	return addrs, nil
}

func (p *workerPool) refresh() error {
	addrs, err := p.resolve()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.addrs = addrs
	p.refreshed = time.Now()
	p.mu.Unlock()
	return nil
}

// pick returns the next worker round-robin. Random selection left workers
// unevenly loaded; with n workers and n jobs it leaves ~37% of them idle.
func (p *workerPool) pick() (string, error) {
	p.mu.RLock()
	addrs, stale := p.addrs, time.Since(p.refreshed) > p.ttl
	p.mu.RUnlock()

	if len(addrs) == 0 || stale {
		if err := p.refresh(); err != nil && len(addrs) == 0 {
			return "", err
		}
		p.mu.RLock()
		addrs = p.addrs
		p.mu.RUnlock()
	}
	if len(addrs) == 0 {
		return "", &net.DNSError{Err: "no workers resolved", Name: p.service}
	}
	i := atomic.AddUint64(&p.next, 1) - 1
	return addrs[i%uint64(len(addrs))], nil
}

// The proxy fans one request out per compile; the default cap of 2 idle
// connections per host would reconnect to a worker for nearly every file.
var workerClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	},
}

func compileHandler(w http.ResponseWriter, r *http.Request, pool *workerPool) {
	filename := r.Header.Get("X-Filename")
	if filename == "" {
		http.Error(w, "missing X-Filename header", http.StatusBadRequest)
		return
	}

	worker, err := pool.pick()
	if err != nil {
		http.Error(w, "no workers available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://"+worker+"/compile", r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-Filename", filename)
	if flags := r.Header.Get("X-Compile-Flags"); flags != "" {
		req.Header.Set("X-Compile-Flags", flags)
	}
	// The payload is gzipped by the client and passed through untouched; without
	// this the worker cannot tell it from a plain translation unit.
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		req.Header.Set("Content-Encoding", enc)
	}
	req.ContentLength = r.ContentLength

	resp, err := workerClient.Do(req)
	if err != nil {
		http.Error(w, "worker request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func main() {
	service := os.Getenv("WORKER_SERVICE")
	if service == "" {
		log.Fatal("WORKER_SERVICE env var is required")
	}
	port := os.Getenv("WORKER_PORT")
	if port == "" {
		port = "8080"
	}
	addr := os.Getenv("ORCHESTRATOR_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	ttl := 10 * time.Second
	if v := os.Getenv("WORKER_REFRESH_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}

	pool := &workerPool{service: service, port: port, ttl: ttl}
	if err := pool.refresh(); err != nil {
		log.Printf("initial worker lookup failed (will retry per request): %v", err)
	}

	http.HandleFunc("/compile", func(w http.ResponseWriter, r *http.Request) {
		compileHandler(w, r, pool)
	})

	log.Println("orchestrator listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
