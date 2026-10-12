// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// LogLineView is a line of a job log, as shown.
type LogLineView struct {
	N    int64
	Step int32
	Text string
	Time string
	// Group marks "##[group]" and "##[endgroup]" lines and Error the
	// "##[error]" ones; the text is shown without the marker.
	Group, End, Error bool
	// Continued marks a piece of a line whose beginning is in an earlier batch
	// of the live feed: the page appends its text to the line before it.
	Continued bool
}

// JobPage is one job with its steps and its log.
type JobPage struct {
	JobRow
	RunHref string
	RunName string
	Run     string
	Steps   []StepView
	Lines   []LogLineView
	// From is the first line shown; Next is the link to the lines after the
	// last shown, "" when it is the end; Follow says the log is still being
	// written, so the page follows it live.
	From    int64
	Next    string
	Prev    string
	Total   int64
	Follow  bool
	RawHref string
	// StreamHref is the live feed of the lines (logStream).
	StreamHref string
	Note       string
}

// appendLogLine adds the line n of a job log to the lines shown. The markers
// of the runner ("##[group]", "##[endgroup]", "##[error]") become structure and
// are not shown as text. A piece that continues a line too long to send whole
// is joined to the line before it; one that opens the list (its line is in an
// earlier batch) keeps Continued, and the page appends it to that line.
func appendLogLine(out []LogLineView, n int64, l ports.JobLogLine) []LogLineView {
	text := l.Text
	v := LogLineView{N: n, Step: l.Step, Time: l.Time.UTC().Format("15:04:05"), Continued: l.Continued}
	switch {
	case strings.HasPrefix(text, "##[group]"):
		v.Group, text = true, strings.TrimPrefix(text, "##[group]")
	case strings.HasPrefix(text, "##[endgroup]"):
		v.End, text = true, strings.TrimPrefix(text, "##[endgroup]")
	case strings.HasPrefix(text, "##[error]"):
		v.Error, text = true, strings.TrimPrefix(text, "##[error]")
	}
	v.Text = visibleText(text)
	if l.Continued && len(out) > 0 {
		out[len(out)-1].Text += v.Text
		return out
	}
	return append(out, v)
}

// logClient is the key of the visitor for the limit of log streams per client.
func (s *Server) logClient(r *http.Request) string {
	return clientlimit.Key(clientlimit.ClientAddr(r, s.cfg.TrustedProxies))
}

// readLog reads up to max lines from a line number, through the log stream of
// the use case, and returns the line after the last read.
func (s *Server) readLog(ctx context.Context, client string, repo domain.RepoName, j ports.JobView, from int64, max int) ([]LogLineView, int64, error) {
	if from > j.Lines || j.Lines == 0 {
		return nil, from, nil
	}
	var out []LogLineView
	next := from
	stop := errors.New("enough")
	err := s.cfg.Runs.GetJobLog(ctx, principal(ctx), repo, ports.GetJobLogParams{JobID: j.ID, Attempt: j.Attempt, Offset: from, Client: client}, func(b ports.LogBatch) error {
		for i, l := range b.Lines {
			if len(out) >= max && !l.Continued {
				return stop // the rest of a cut line still belongs to this page
			}
			out = appendLogLine(out, b.Offset+int64(i), l)
			next = b.Offset + int64(i) + 1
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) && !errors.Is(err, domain.ErrNotFound) {
		return nil, from, err
	}
	return out, next, nil
}

// rawLog is GET /<repo>/actions/runs/<id>/job/<job id>/raw: the log as text,
// one line per line with its time, joined where the runner cut a long line.
func (s *Server) rawLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	id, err := parseRunPath(r)
	jobID := r.PathValue("job")
	if runOf, _, _, ok := domain.ParseJobID(jobID); err != nil || !ok || runOf != id {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	var attempt int32
	if a := r.URL.Query().Get("attempt"); a != "" {
		n, perr := strconv.ParseInt(a, 10, 32)
		if perr != nil || n < 1 {
			s.fail(ctx, w, r, errNotFound)
			return
		}
		attempt = int32(n)
	}
	// The job is read first: a job that is not there, or not readable, is a 404
	// before any header of the log is sent.
	if _, err := s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, jobID, attempt); err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", fmt.Sprintf(`inline; filename="job-%s.log"`, jobID))
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	written := 0
	open := false
	err = s.cfg.Runs.GetJobLog(ctx, principal(ctx), d.repo.Name, ports.GetJobLogParams{JobID: jobID, Attempt: attempt, Client: s.logClient(r)}, func(b ports.LogBatch) error {
		for _, l := range b.Lines {
			if written > rawLogMaxBytes {
				return errors.New("log too large")
			}
			var line string
			switch {
			case l.Continued && open:
				line = l.Text
			case open:
				line = "\n" + l.Time.UTC().Format(time.RFC3339Nano) + " " + l.Text
			default:
				line = l.Time.UTC().Format(time.RFC3339Nano) + " " + l.Text
			}
			open = true
			n, werr := fmt.Fprint(w, line)
			written += n
			if werr != nil {
				return werr
			}
		}
		return nil
	})
	if open {
		_, _ = fmt.Fprintln(w)
	}
	if err != nil && !open && errors.Is(err, domain.ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
	}
}

// logClass is the class of a line of the log.
func logClass(ln LogLineView) string {
	switch {
	case ln.Group:
		return "lg group"
	case ln.End:
		return "lg endgroup"
	case ln.Error:
		return "lg error"
	}
	return "lg"
}

// logStream is GET /<repo>/actions/runs/<id>/job/<job id>/stream?from=N: the
// lines of a job log from line N as Server-Sent Events, following the job while
// it runs. Each "ci-log" event holds the <li> of the lines of one batch and has
// the number of the next line as its id, so that a reconnection (Last-Event-ID)
// goes on where it stopped. "ci-end" tells the job is over and every line was
// sent. The job events of the repository stream carry no line (see events.go).
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		s.denyEvents(ctx, w, err)
		return
	}
	id, err := parseRunPath(r)
	jobID := r.PathValue("job")
	if runOf, _, _, ok := domain.ParseJobID(jobID); err != nil || !ok || runOf != id {
		s.denyEvents(ctx, w, errNotFound)
		return
	}
	from := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		from, err = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("from"); v != "" {
		from, err = strconv.ParseInt(v, 10, 64)
	}
	if err != nil || from < 0 {
		s.denyEvents(ctx, w, errNotFound)
		return
	}
	if _, err := s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, jobID, 0); err != nil {
		s.denyEvents(ctx, w, err)
		return
	}

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	cacheHeaders(ctx, hdr, "no-cache, no-transform", sessionCache+", no-transform")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &sseWriter{w: w, rc: rc}
	if out.raw(": connected\n\n") != nil {
		return
	}
	n := from
	err = s.cfg.Runs.GetJobLog(ctx, principal(ctx), d.repo.Name, ports.GetJobLogParams{
		JobID: jobID, Offset: from, Follow: true,
		Client: s.logClient(r),
	}, func(b ports.LogBatch) error {
		var views []LogLineView
		for i, l := range b.Lines {
			views = appendLogLine(views, b.Offset+int64(i), l)
		}
		var html strings.Builder
		for _, v := range views {
			var b2 strings.Builder
			if err := LogLine(v).Render(ctx, &b2); err != nil {
				return err
			}
			html.WriteString(strings.ReplaceAll(b2.String(), "\n", ""))
		}
		n = b.Offset + int64(len(b.Lines))
		_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
		_, werr := io.WriteString(w, "event: ci-log\nid: "+strconv.FormatInt(n, 10)+"\ndata: "+html.String()+"\n\n")
		if werr != nil {
			return werr
		}
		return rc.Flush()
	})
	if err == nil || errors.Is(err, domain.ErrNotFound) {
		_ = out.raw("event: ci-end\nid: " + strconv.FormatInt(n, 10) + "\ndata: end\n\n")
	}
}
