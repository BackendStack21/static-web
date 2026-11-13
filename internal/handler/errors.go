package handler

import "github.com/valyala/fasthttp"

// errorContentType is the Content-Type of every writeError response.
const errorContentType = "text/plain; charset=utf-8"

// representationHeaders describe the successful response a handler may have
// started to build before it failed. They are wrong on an error body (a
// plain-text 500 must not claim Content-Encoding: gzip or Content-Range, and
// a 403 must not carry Cache-Control: public, max-age=… or an ETag that lets
// a cache keep it), so writeError removes them.
var representationHeaders = [...]string{
	"Content-Encoding",
	"Content-Range",
	"Content-Length",
	"ETag",
	"Last-Modified",
	"Cache-Control",
	"Accept-Ranges",
	"X-Cache",
}

// writeError replaces the response with a plain-text error and is the
// header-preserving replacement for fasthttp's ctx.Error in this package.
//
// ctx.Error calls Response.Reset, which also drops every header that outer
// middleware has already set: the security headers (Content-Security-Policy,
// X-Content-Type-Options, X-Frame-Options, Referrer-Policy,
// Permissions-Policy), the CORS headers and Vary, and Allow (HTTP-F4).
// writeError keeps those. It
//   - sets the status code to code,
//   - sets Content-Type to "text/plain; charset=utf-8",
//   - replaces the body with msg, detaching any body stream,
//   - removes the representation headers of a half-built success response:
//     Content-Encoding, Content-Range, Content-Length, ETag, Last-Modified,
//     Cache-Control, Accept-Ranges and X-Cache.
//
// Every other header is left as it is. Content-Length is recomputed from msg
// when the response is written.
//
// msg is sent to the client verbatim, so it must not contain internal
// details such as filesystem paths or configured limits.
func writeError(ctx *fasthttp.RequestCtx, msg string, code int) {
	h := &ctx.Response.Header
	for _, k := range representationHeaders {
		h.Del(k)
	}
	ctx.SetStatusCode(code)
	h.SetContentType(errorContentType)
	ctx.Response.SetBodyString(msg)
}
