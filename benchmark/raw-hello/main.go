// Bare-minimum Go HTTP server: pre-allocated response, no middleware, no allocs.
// This establishes the net/http ceiling on this machine.
package main

import (
	"net/http"
	"os"
)

var body = []byte("Hello, World!")

func main() {
	port := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		port = ":" + p
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "13")
		w.WriteHeader(200)
		w.Write(body)
	})

	http.ListenAndServe(port, mux)
}
