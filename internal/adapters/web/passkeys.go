// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner's passkeys on the web (ADR 0026).
//
// The pages are GET: /-/login (sign in, anonymous) and /-/passkeys (the
// owner's list). The ceremonies are JSON POSTs the script static/js/passkey.js
// makes, each in two steps (begin, finish). Every POST passes the gate: an
// Origin equal to the public origin, and a session, except the two login
// endpoints, which are how a session starts. The finish endpoints also need
// "Content-Type: application/json", which a cross-site form cannot send.

const (
	passkeyLoginPath = "/-/login"
	passkeysPath     = "/-/passkeys"

	loginBeginPath  = passkeysPath + "/login/begin"
	loginFinishPath = passkeysPath + "/login/finish"

	// maxCeremonyBytes bounds the body of a finish: a response of an
	// authenticator is well under 10 KiB.
	maxCeremonyBytes = 64 << 10
)

// passkeysOn tells that the passkey routes exist.
func (s *Server) passkeysOn() bool { return s.cfg.Passkeys != nil && s.cfg.Sessions != nil }

func (s *Server) passkeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+passkeyLoginPath, s.passkeyLoginPage)
	mux.HandleFunc("POST "+loginBeginPath, s.passkeyLoginBegin)
	mux.HandleFunc("POST "+loginFinishPath, s.passkeyLoginFinish)
	mux.HandleFunc("GET "+passkeysPath, s.page(s.passkeysPage))
	mux.HandleFunc("POST "+passkeysPath+"/register/begin", s.passkeyRegisterBegin)
	mux.HandleFunc("POST "+passkeysPath+"/register/finish", s.passkeyRegisterFinish)
	mux.HandleFunc("POST "+passkeysPath+"/{id}/remove", s.passkeyRemove)
}

// isPasskeyLoginPath tells whether u is exactly one of the two endpoints an
// anonymous browser may post to. Like isLoginPath it looks at the escaped path,
// which is what the router matches.
func isPasskeyLoginPath(path string) bool {
	return path == loginBeginPath || path == loginFinishPath
}

// jsonReply writes a JSON answer that no cache keeps.
func jsonReply(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", noStore)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type jsonMessage struct {
	Error string `json:"error"`
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonReply(w, status, jsonMessage{Error: msg})
}

// jsonInternal answers a failure that is not the caller's to fix: an id for the
// log, and no detail.
func (s *Server) jsonInternal(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	id := requestID()
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web passkey request failed")
	jsonError(w, http.StatusInternalServerError, "internal error, request id "+id)
}

// jsonBody checks the content type of a finish and reads its body, bounded. It
// answers 415 or 413 and returns false when it cannot.
func jsonBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		jsonError(w, http.StatusUnsupportedMediaType, "send application/json")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCeremonyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			jsonError(w, http.StatusBadRequest, "request could not be read")
		}
		return nil, false
	}
	return body, true
}

// ceremonyReply is the answer of a begin.
type ceremonyReply struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

// blocked answers 429 when the client has failed too many sign-ins.
func (s *Server) blocked(w http.ResponseWriter, r *http.Request) (addr string, blocked bool) {
	addr = clientlimit.ClientAddr(r, s.cfg.TrustedProxies)
	if is, wait := s.cfg.Limiter.Blocked(r.Context(), addr); is {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		jsonError(w, http.StatusTooManyRequests, "too many attempts")
		return addr, true
	}
	return addr, false
}

// passkeyLoginPage is GET /-/login: the sign-in page. A signed-in visitor goes
// home.
func (s *Server) passkeyLoginPage(w http.ResponseWriter, r *http.Request) {
	if SignedIn(r.Context()) {
		seeOther(w, r, "/")
		return
	}
	l := s.stateLayout("sign in", []Crumb{{Label: "sign in", Current: true}})
	s.secretPage(w, r, http.StatusOK, PasskeyLogin(l, PasskeyLoginPage{
		Title:  "sign in",
		Button: "Sign in with a passkey",
		Lines:  []string{"or run `ermite web login` in a terminal."},
		Begin:  loginBeginPath,
		Finish: loginFinishPath,
	}))
}

// passkeyLoginBegin is POST /-/passkeys/login/begin.
func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	addr, blocked := s.blocked(w, r)
	if blocked {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	ceremony, options, err := s.cfg.Passkeys.BeginLogin(ctx, addr)
	if err != nil {
		s.jsonInternal(ctx, w, r, err)
		return
	}
	jsonReply(w, http.StatusOK, ceremonyReply{Ceremony: ceremony, Options: options})
}

// finishRequest is the body of a finish.
type finishRequest struct {
	Ceremony   string          `json:"ceremony"`
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}

// passkeyLoginFinish is POST /-/passkeys/login/finish: it checks the response
// of the authenticator and sets the session cookie, the same as a login link.
// A refusal counts as a failed authentication of the client, whatever the
// reason.
func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	// The limiter first: a client that is blocked gets 429 whatever it sends.
	addr, blocked := s.blocked(w, r)
	if blocked {
		return
	}
	// A wrong content type or a malformed body is a bug of the client, not a
	// guess at the owner's passkey: it answers 4xx and is not counted as a failure.
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req finishRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Ceremony == "" || len(req.Credential) == 0 {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	token, lifetime, err := s.cfg.Passkeys.FinishLogin(ctx, req.Ceremony, req.Credential)
	switch {
	case errors.Is(err, ports.ErrLinkRejected):
		s.cfg.Limiter.Fail(ctx, addr)
		jsonError(w, http.StatusUnauthorized, "passkey not accepted")
		return
	case err != nil:
		s.jsonInternal(ctx, w, r, err)
		return
	}
	h := w.Header()
	h.Del("Set-Cookie") // a stale cookie cleared by withSession
	http.SetCookie(w, s.cookie(token, int(lifetime.Seconds())))
	h.Set("Cache-Control", noStore)
	w.WriteHeader(http.StatusNoContent)
}

// passkeysPage is GET /-/passkeys: the owner's list. Anybody else gets the 404
// page, as for any page that does not exist for them.
func (s *Server) passkeysPage(ctx context.Context, _ *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	list, err := s.cfg.Passkeys.List(ctx, principal(ctx))
	if err != nil {
		return nil, 0, err
	}
	now := s.cfg.Now()
	rows := make([]PasskeyRow, 0, len(list))
	for _, p := range list {
		rows = append(rows, PasskeyRow{
			Name:    p.Name,
			Created: ago(now, p.CreatedAt), CreatedAt: rfc3339(p.CreatedAt),
			LastUsed: ago(now, p.LastUsedAt), LastUsedAt: rfc3339(p.LastUsedAt),
			Synced: p.BackupState,
			Remove: passkeysPath + "/" + base64.RawURLEncoding.EncodeToString(p.ID) + "/remove",
		})
	}
	l := s.base()
	l.Title = "passkeys · " + s.instance
	l.Crumbs = []Crumb{{Label: "passkeys", Current: true}}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	return Passkeys(l, PasskeysPage{
		Rows: rows, Full: len(list) >= domain.MaxPasskeys, Max: domain.MaxPasskeys,
		Begin: passkeysPath + "/register/begin", Finish: passkeysPath + "/register/finish",
	}), http.StatusOK, nil
}

// registerError answers a refused registration step.
func (s *Server) registerError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		// The browser is gone.
	case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrUnauthenticated) && !SignedIn(ctx):
		jsonError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, domain.ErrAlreadyExists):
		jsonError(w, http.StatusConflict, "this passkey is already registered")
	case errors.Is(err, domain.ErrResourceExhausted):
		jsonError(w, http.StatusTooManyRequests, rule(err.Error(), "a limit is reached"))
	case domain.IsInvalid(err):
		jsonError(w, http.StatusBadRequest, rule(err.Error(), "the name is not valid"))
	case errors.Is(err, domain.ErrUnauthenticated):
		jsonError(w, http.StatusBadRequest, "passkey not accepted, try again")
	default:
		s.jsonInternal(ctx, w, r, err)
	}
}

// passkeyRegisterBegin is POST /-/passkeys/register/begin. Like the finish
// endpoints it needs "Content-Type: application/json".
func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	// {"name": "..."}; an empty body is no name. The name is checked now, before
	// the browser prompts the owner for a passkey that would then be refused.
	var req struct {
		Name string `json:"name"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "malformed request")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	ceremony, options, err := s.cfg.Passkeys.BeginRegistration(ctx, principal(ctx), req.Name)
	if err != nil {
		s.registerError(ctx, w, r, err)
		return
	}
	jsonReply(w, http.StatusOK, ceremonyReply{Ceremony: ceremony, Options: options})
}

// passkeyRegisterFinish is POST /-/passkeys/register/finish.
func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req finishRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Ceremony == "" || len(req.Credential) == 0 {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := s.cfg.Passkeys.FinishRegistration(ctx, principal(ctx), req.Ceremony, req.Name, req.Credential); err != nil {
		s.registerError(ctx, w, r, err)
		return
	}
	w.Header().Set("Cache-Control", noStore)
	w.WriteHeader(http.StatusNoContent)
}

// passkeyRemove is POST /-/passkeys/{id}/remove: it removes the passkey, which
// signs every browser out, this one included: the answer sends it to the
// sign-in page.
func (s *Server) passkeyRemove(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 {
		s.actionFailed(ctx, w, r, errNotFound, passkeysPath)
		return
	}
	if err := s.cfg.Passkeys.Remove(ctx, principal(ctx), id); err != nil {
		s.actionFailed(ctx, w, r, err, passkeysPath)
		return
	}
	seeOther(w, r, passkeyLoginPath)
}
