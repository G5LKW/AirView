package server

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// Per-request deadlines (SEC-05). net/http's ReadHeaderTimeout covers slow
// headers only; a client that sends headers and then trickles a body would
// otherwise hold a goroutine and a descriptor indefinitely.
const (
	// bodyReadTimeout bounds reading a request body (except the long
	// upload routes, Route.LongBody).
	bodyReadTimeout = 30 * time.Second
	// jsonWriteTimeout bounds writing a JSON API answer to a slow reader.
	jsonWriteTimeout = 2 * time.Minute
)

// bodyDeadline sets the read deadline for the request body and, for JSON
// API routes, the write deadline. The read deadline is lifted as soon as
// the body has been read to its end (net/http then watches the connection
// for a client disconnect, which must not time out while the handler
// works). Requests without a body get no read deadline for the same
// reason. Errors (a ResponseWriter that does not support deadlines, as in
// tests) are ignored.
func (s *Server) bodyDeadline(w http.ResponseWriter, r *http.Request, rt *Route) *http.Request {
	return s.bodyDeadlineFor(w, r, rt, bodyReadTimeout)
}

func (s *Server) bodyDeadlineFor(w http.ResponseWriter, r *http.Request, rt *Route, d time.Duration) *http.Request {
	rc := http.NewResponseController(w)
	if rt.API != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(jsonWriteTimeout))
	}
	if rt.LongBody || r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return r
	}
	if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return r
	}
	r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
	return r
}

// deadlineBody lifts the read deadline once the body has been read to EOF.
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	done bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !b.done {
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}
