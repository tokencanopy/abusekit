package serve

// White-box unit tests for recoverMiddleware/headerTrackingWriter (R8,
// round 2 fix round). These two behaviors are fundamentally unreachable
// from serve_test's black-box HTTP tests (this package's usual
// convention — codebase-design's "tests cross the HTTP seam, not
// internals"): there is no legitimate way to make a REAL handler panic
// with http.ErrAbortHandler specifically, or panic AFTER partially
// writing a response, from outside. A minimal internal package test,
// constructing recoverMiddleware directly around a synthetic panicking
// handler, is the only way to exercise this safety net at all.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverMiddleware_CleanEnvelopeOnOrdinaryPanic(t *testing.T) {
	s := &Server{}
	h := s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatalf("expected a JSON error envelope body, got no Content-Type at all")
	}
}

// TestRecoverMiddleware_RepanicsErrAbortHandler is R8's own fix: net/http's
// sentinel for "abort this handler, don't log, don't write anything" must
// propagate unchanged, not be swallowed into an ordinary 500 envelope.
func TestRecoverMiddleware_RepanicsErrAbortHandler(t *testing.T) {
	s := &Server{}
	h := s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()

	defer func() {
		got := recover()
		if got != http.ErrAbortHandler {
			t.Fatalf("expected http.ErrAbortHandler to propagate out of ServeHTTP, got %v", got)
		}
	}()
	h.ServeHTTP(rec, req)
	t.Fatalf("expected ServeHTTP to panic with http.ErrAbortHandler, it returned normally")
}

// TestRecoverMiddleware_DoesNotOverwriteAlreadySentResponse is R8's other
// fix: a handler that panics AFTER already writing (part of) a response
// must not have the error envelope appended on top — the already-sent
// bytes are left exactly as they are.
func TestRecoverMiddleware_DoesNotOverwriteAlreadySentResponse(t *testing.T) {
	s := &Server{}
	h := s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial-body"))
		panic("boom after headers")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the already-sent header, not a rewritten 500)", rec.Code)
	}
	if rec.Body.String() != "partial-body" {
		t.Fatalf("body = %q, want exactly the already-written bytes with nothing appended", rec.Body.String())
	}
}

// TestRecoverMiddleware_DoesNotOverwriteImplicitHeader covers the OTHER
// way a response can already have "started": a Write with no preceding
// explicit WriteHeader implicitly sends a 200 — headerTrackingWriter must
// catch this case too, not just an explicit WriteHeader call.
func TestRecoverMiddleware_DoesNotOverwriteImplicitHeader(t *testing.T) {
	s := &Server{}
	h := s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("implicit-200-body"))
		panic("boom after an implicit WriteHeader")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the implicit header from the first Write)", rec.Code)
	}
	if rec.Body.String() != "implicit-200-body" {
		t.Fatalf("body = %q, want exactly the already-written bytes with nothing appended", rec.Body.String())
	}
}
