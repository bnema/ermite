// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"io"
	"net/http"
	"sync"

	"bnema.dev/ermite/internal/domain"
)

// inProcessHost is the host of the URLs of in-process calls. The name is
// never resolved.
const inProcessHost = "ermite.invalid"

// InProcessURL is the base URL to give to the generated Connect clients that
// use InProcessClient.
const InProcessURL = "http://" + inProcessHost

// InProcessClient returns an HTTP client whose requests are served by the
// handlers of the API in this process: no socket, no network hop. Build the
// generated Connect clients on it:
//
//	hc := api.InProcessClient()
//	repos := ermiteconnect.NewRepoServiceClient(hc, rpc.InProcessURL)
//
// The caller is the principal found in the context of each call
// (domain.ContextWithPrincipal), anonymous when there is none; the
// Authorization header is ignored. This is the identity of the web (P5):
// anonymous for visitors, the owner for a session. Only code in this process
// can put a principal in a context, which is why the interceptor trusts it.
// Unary and server-streaming calls work, with all the codecs; the request
// log and the error mapping are those of remote calls.
func (a *API) InProcessClient() *http.Client {
	return &http.Client{Transport: inProcessTransport{h: a.handler}}
}

type inProcessTransport struct{ h http.Handler }

// RoundTrip serves req with the handler in a goroutine, and returns once the
// handler wrote its status. The body of the response is a pipe the handler
// writes to, so that streams are delivered as they are sent.
func (t inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		p = domain.Anonymous()
	}
	sreq := req.Clone(domain.ContextWithPrincipal(ctx, p))
	sreq.RemoteAddr = "in-process"
	sreq.RequestURI = req.URL.RequestURI()
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	sreq.Header.Del("Authorization")

	pr, pw := io.Pipe()
	w := &pipeResponse{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.writeHeader(http.StatusInternalServerError)
				_ = pw.CloseWithError(io.ErrUnexpectedEOF)
			}
		}()
		t.h.ServeHTTP(w, sreq)
		w.writeHeader(http.StatusOK) // a handler that wrote nothing
		_ = pw.Close()
	}()

	select {
	case <-w.ready:
	case <-ctx.Done():
		_ = pr.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
	return &http.Response{
		Status:        http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// pipeResponse is the http.ResponseWriter of an in-process call.
type pipeResponse struct {
	header http.Header
	pw     *io.PipeWriter

	once   sync.Once
	ready  chan struct{}
	status int
	sent   http.Header // header as of WriteHeader
}

func (w *pipeResponse) Header() http.Header { return w.header }

func (w *pipeResponse) WriteHeader(code int) { w.writeHeader(code) }

func (w *pipeResponse) writeHeader(code int) {
	w.once.Do(func() {
		w.status, w.sent = code, w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeResponse) Write(b []byte) (int, error) {
	w.writeHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush implements http.Flusher, which streaming handlers require: the pipe
// delivers every write at once, there is nothing to flush but the header.
func (w *pipeResponse) Flush() { w.writeHeader(http.StatusOK) }
