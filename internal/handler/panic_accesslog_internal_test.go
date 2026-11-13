package handler

import (
	"log"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// A request whose handler panics must still get an access-log line, with
// the 500 it was answered with. Recovery sat outside the logging middleware,
// so the panic unwound through it and the request was never logged.
func TestAccessLog_RecordsPanickedRequest(t *testing.T) {
	var out alRecordingWriter
	logger := log.New(&out, "", 0)
	logging := func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return loggingMiddlewareWithWriter(next, logger)
	}
	panicking := func(ctx *fasthttp.RequestCtx) { panic("boom") }
	h := withAccessLogAndRecovery(panicking, logging)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/explode")
	h(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	lines := out.lines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "GET /explode 500 ") {
		t.Errorf("access log = %q, want one line for GET /explode 500", lines)
	}

	// A panic in the logging middleware itself is still recovered.
	h = withAccessLogAndRecovery(func(*fasthttp.RequestCtx) {}, func(fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(*fasthttp.RequestCtx) { panic("logger broke") }
	})
	var ctx2 fasthttp.RequestCtx
	h(&ctx2)
	if got := ctx2.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Errorf("panic in logging: status = %d, want 500", got)
	}
}
