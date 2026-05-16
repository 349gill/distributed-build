package main

import (
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
)

func pickWorker(service, port string) (string, error) {
	ips, err := net.LookupHost(service)
	if err != nil {
		return "", err
	}
	ip := ips[rand.Intn(len(ips))]
	return net.JoinHostPort(ip, port), nil
}

func compileHandler(w http.ResponseWriter, r *http.Request, service, port string) {
	filename := r.Header.Get("X-Filename")
	if filename == "" {
		http.Error(w, "missing X-Filename header", http.StatusBadRequest)
		return
	}

	worker, err := pickWorker(service, port)
	if err != nil {
		http.Error(w, "no workers available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+worker+"/compile", r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-Filename", filename)
	req.ContentLength = r.ContentLength

	resp, err := http.DefaultClient.Do(req)
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

	http.HandleFunc("/compile", func(w http.ResponseWriter, r *http.Request) {
		compileHandler(w, r, service, port)
	})

	log.Println("orchestrator listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
