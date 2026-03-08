// Bare-minimum fasthttp server: pre-allocated response, no middleware, no allocs.
// Direct comparison against the net/http hello world to isolate HTTP stack overhead.
package main

import (
	"log"
	"os"

	"github.com/valyala/fasthttp"
)

var (
	body        = []byte("Hello, World!")
	contentType = []byte("text/plain")
	contentLen  = []byte("13")
)

func handler(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.SetBytesV("Content-Type", contentType)
	ctx.Response.Header.SetBytesV("Content-Length", contentLen)
	ctx.SetStatusCode(200)
	ctx.SetBody(body)
}

func main() {
	port := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		port = ":" + p
	}

	s := &fasthttp.Server{
		Handler: handler,
		Name:    "fasthttp-hello",
	}

	log.Printf("fasthttp listening on %s", port)
	if err := s.ListenAndServe(port); err != nil {
		log.Fatal(err)
	}
}
