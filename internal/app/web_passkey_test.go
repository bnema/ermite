// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

const passkeyOrigin = "https://git.example.test"

var passkeyRP = virtualwebauthn.RelyingParty{ID: "git.example.test", Name: "git.example.test", Origin: passkeyOrigin}

// passkeyHarness is a forge whose public URL allows passkeys, with a virtual
// authenticator in front of its real web handler.
type passkeyHarness struct {
	*cliHarness
	b    browser
	auth virtualwebauthn.Authenticator
	cred virtualwebauthn.Credential
}

func newPasskeyHarness(t *testing.T) *passkeyHarness {
	t.Helper()
	h := newHarness(t, harnessOpts{publicURL: passkeyOrigin})
	c := &cliHarness{harness: h, url: h.srv.URL}
	return &passkeyHarness{
		cliHarness: c, b: browser{h},
		auth: virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true}),
		cred: virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
	}
}

// postJSON posts a JSON body as the browser's script does.
func (p *passkeyHarness) postJSON(path, token string, body any) page {
	p.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(p.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest("POST", p.url+path, &buf)
	require.NoError(p.t, err)
	req.Header.Set("Origin", passkeyOrigin)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Cookie", sessionCookie+"="+token)
	}
	rec, err := http.DefaultClient.Do(req)
	require.NoError(p.t, err)
	defer func() { _ = rec.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(rec.Body)
	return page{status: rec.StatusCode, hdr: rec.Header, body: out.String()}
}

// passkeyLoginPath turns the link the CLI printed into a path on the test
// server (the harness here has an https public URL).
func passkeyLoginPath(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(strings.TrimSpace(link))
	require.NoError(t, err)
	assert.Equal(t, passkeyOrigin, u.Scheme+"://"+u.Host, "the link is on the public URL")
	return u.Path
}

type beginReply struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

// register adds the authenticator's credential as the owner holding token.
func (p *passkeyHarness) register(token, name string) page {
	p.t.Helper()
	begin := p.postJSON("/-/passkeys/register/begin", token, map[string]any{"name": name})
	require.Equal(p.t, http.StatusOK, begin.status, begin.body)
	var br beginReply
	require.NoError(p.t, json.Unmarshal([]byte(begin.body), &br))
	opts, err := virtualwebauthn.ParseAttestationOptions(string(br.Options))
	require.NoError(p.t, err)
	resp := virtualwebauthn.CreateAttestationResponse(passkeyRP, p.auth, p.cred, *opts)
	fin := p.postJSON("/-/passkeys/register/finish", token, map[string]any{"ceremony": br.Ceremony, "credential": json.RawMessage(resp)})
	if fin.status == http.StatusNoContent {
		p.auth.AddCredential(p.cred)
		p.auth.Options.UserHandle = p.userHandle()
	}
	return fin
}

// userHandle reads the owner's WebAuthn user handle, which a real
// authenticator got from the creation options.
func (p *passkeyHarness) userHandle() []byte {
	h, err := p.forge.store.Passkeys().UserHandle(p.ctx)
	require.NoError(p.t, err)
	return h
}

// login signs in with the authenticator; it returns the answer of finish.
func (p *passkeyHarness) login() page {
	p.t.Helper()
	begin := p.postJSON("/-/passkeys/login/begin", "", nil)
	require.Equal(p.t, http.StatusOK, begin.status, begin.body)
	var br beginReply
	require.NoError(p.t, json.Unmarshal([]byte(begin.body), &br))
	opts, err := virtualwebauthn.ParseAssertionOptions(string(br.Options))
	require.NoError(p.t, err)
	resp := virtualwebauthn.CreateAssertionResponse(passkeyRP, p.auth, p.cred, *opts)
	return p.postJSON("/-/passkeys/login/finish", "", map[string]any{"ceremony": br.Ceremony, "credential": json.RawMessage(resp)})
}

// AC1, AC2, AC3: the owner signs in with the CLI link, adds a passkey, signs
// out, signs in with the passkey, sees it in the CLI, and removing it ends
// every session.
func TestPasskeyEndToEnd(t *testing.T) {
	p := newPasskeyHarness(t)
	p.seed("secret", domain.VisibilityPrivate, "notes.txt")

	// The server announces passkeys, and the pages link to them.
	ver := p.cli(t, "", "version", "--json").ok(t)
	assert.Contains(t, ver.stdout, `"passkeys"`)
	assert.Empty(t, ver.stderr, "no feature is missing")
	login := p.b.get("/-/login", nil)
	require.Equal(t, http.StatusOK, login.status)
	assert.Contains(t, login.body, `data-passkey="login"`)

	// Anonymous: the list is a 404, registration is refused.
	assert.Equal(t, http.StatusNotFound, p.b.get("/-/passkeys", nil).status)
	assert.Equal(t, http.StatusForbidden, p.postJSON("/-/passkeys/register/begin", "", map[string]any{}).status)

	// Sign in with the CLI link (the way in before any passkey), add a passkey.
	link := p.cli(t, adminToken, "web", "login").ok(t)
	pg := p.b.do("POST", passkeyLoginPath(t, link.stdout), map[string]string{"Origin": passkeyOrigin}, "")
	require.Equal(t, http.StatusSeeOther, pg.status, pg.body)
	token := pg.cookie(sessionCookie).Value

	fin := p.register(token, "Test phone")
	require.Equal(t, http.StatusNoContent, fin.status, fin.body)

	list := p.b.get("/-/passkeys", as(token))
	require.Equal(t, http.StatusOK, list.status)
	assert.Contains(t, list.body, "Test phone")
	assert.Equal(t, "private, no-store", list.hdr.Get("Cache-Control"))

	// The same credential cannot be added twice.
	dup := p.register(token, "again")
	assert.Equal(t, http.StatusConflict, dup.status, dup.body)

	// AC2: an anonymous browser signs in with the passkey: same cookie.
	res := p.login()
	require.Equal(t, http.StatusNoContent, res.status, res.body)
	var ck *http.Cookie
	for _, c := range (&http.Response{Header: res.hdr}).Cookies() {
		if c.Name == sessionCookie {
			ck = c
		}
	}
	require.NotNil(t, ck)
	assert.True(t, ck.Secure)
	assert.True(t, ck.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, ck.SameSite)
	assert.Equal(t, 12*3600, ck.MaxAge)
	home := p.b.get("/", as(ck.Value))
	assert.Contains(t, home.body, "secret", "the passkey session is the owner's")

	// AC3: the CLI lists it, with its use; the id removes it.
	out := p.cli(t, adminToken, "web", "passkey", "list", "--json").ok(t)
	var listed []struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		LastUsedAt *string `json:"last_used_at"`
		Synced     bool    `json:"synced"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, "Test phone", listed[0].Name)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(p.cred.ID), listed[0].ID)
	assert.NotNil(t, listed[0].LastUsedAt, "the sign-in was recorded")

	rm := p.cli(t, adminToken, "web", "passkey", "remove", listed[0].ID).ok(t)
	assert.Contains(t, rm.stderr, "every browser is signed out")
	for _, tok := range []string{token, ck.Value} {
		assert.Equal(t, http.StatusNotFound, p.b.get("/-/passkeys", as(tok)).status, "the epoch moved: both sessions are gone")
	}
	// The removed passkey no longer signs in, and is not there to remove again.
	res = p.login()
	assert.Equal(t, http.StatusUnauthorized, res.status, res.body)
	assert.Empty(t, res.hdr.Values("Set-Cookie"))
	again := p.cli(t, adminToken, "web", "passkey", "remove", listed[0].ID)
	require.Error(t, again.err)
	assert.Contains(t, again.err.Error(), "not found")
}

// A passkey that signs in is refused when the origin is wrong, and nothing is set.
func TestPasskeyLoginRefusedFromAnotherOrigin(t *testing.T) {
	p := newPasskeyHarness(t)
	link := p.cli(t, adminToken, "web", "login").ok(t)
	pg := p.b.do("POST", passkeyLoginPath(t, link.stdout), map[string]string{"Origin": passkeyOrigin}, "")
	token := pg.cookie(sessionCookie).Value
	require.Equal(t, http.StatusNoContent, p.register(token, "k").status)

	// The Origin header is wrong: the gate refuses before anything else.
	req, err := http.NewRequest("POST", p.url+"/-/passkeys/login/begin", strings.NewReader(""))
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The authenticator answers for another relying party.
	begin := p.postJSON("/-/passkeys/login/begin", "", nil)
	var br beginReply
	require.NoError(t, json.Unmarshal([]byte(begin.body), &br))
	opts, err := virtualwebauthn.ParseAssertionOptions(string(br.Options))
	require.NoError(t, err)
	evil := virtualwebauthn.RelyingParty{ID: "git.example.test", Name: "x", Origin: "https://evil.example"}
	resp2 := virtualwebauthn.CreateAssertionResponse(evil, p.auth, p.cred, *opts)
	fin := p.postJSON("/-/passkeys/login/finish", "", map[string]any{"ceremony": br.Ceremony, "credential": json.RawMessage(resp2)})
	assert.Equal(t, http.StatusUnauthorized, fin.status, fin.body)
	assert.Empty(t, fin.hdr.Values("Set-Cookie"))

	// The ceremony is burnt: even the right answer is refused afterwards.
	good := virtualwebauthn.CreateAssertionResponse(passkeyRP, p.auth, p.cred, *opts)
	fin = p.postJSON("/-/passkeys/login/finish", "", map[string]any{"ceremony": br.Ceremony, "credential": json.RawMessage(good)})
	assert.Equal(t, http.StatusUnauthorized, fin.status)
}

// AC4: on the harness's http public URL (not https), passkeys are off: no
// route, no link, no feature, and the RPCs say so.
func TestPasskeysDisabledOnHTTPPublicURL(t *testing.T) {
	c := newCLIHarness(t) // http://example.test
	b := browser{c.harness}
	assert.Equal(t, http.StatusNotFound, b.get("/-/login", nil).status)
	assert.Equal(t, http.StatusForbidden, b.do("POST", "/-/passkeys/login/begin", map[string]string{"Origin": publicOrigin}, "").status, "no session, no route")
	token := c.signIn(t)
	assert.Equal(t, http.StatusNotFound, b.get("/-/passkeys", as(token)).status)
	home := b.get("/", as(token))
	assert.NotContains(t, home.body, "/-/passkeys")
	assert.NotContains(t, b.get("/", nil).body, `href="/-/login"`)

	res := c.cli(t, adminToken, "web", "passkey", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "passkeys are not available on this server")
	ver := c.cli(t, "", "version", "--json").ok(t)
	assert.NotContains(t, ver.stdout, `"features": [`+"\n"+`      "passkeys"`)
}

// A passkey whose counter does not advance (a hardware key that was cloned) is
// refused end to end, with nothing set and the stored counter kept, while a
// counter that advances signs in. Without the sign-count check of the use case
// the clone would sign in: the WebAuthn adapter accepts it and only reports the
// stored counter back.
func TestPasskeyClonedAuthenticator(t *testing.T) {
	p := newPasskeyHarness(t)
	link := p.cli(t, adminToken, "web", "login").ok(t)
	pg := p.b.do("POST", passkeyLoginPath(t, link.stdout), map[string]string{"Origin": passkeyOrigin}, "")
	token := pg.cookie(sessionCookie).Value

	// A hardware key: its counter counts up from 1.
	p.cred.Counter = 1
	require.Equal(t, http.StatusNoContent, p.register(token, "key").status)

	p.cred.Counter = 5
	res := p.login()
	require.Equal(t, http.StatusNoContent, res.status, res.body)

	// The clone presents the counter it last saw: not higher than the stored 5.
	for _, counter := range []uint32{5, 3} {
		p.cred.Counter = counter
		res = p.login()
		assert.Equal(t, http.StatusUnauthorized, res.status, "counter %d: %s", counter, res.body)
		assert.Empty(t, res.hdr.Values("Set-Cookie"), "no session for a clone")
	}

	// The refusals recorded nothing: the next higher counter still works.
	p.cred.Counter = 6
	res = p.login()
	assert.Equal(t, http.StatusNoContent, res.status, res.body)

	stored, err := p.forge.store.Passkeys().List(p.ctx)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, uint32(6), stored[0].SignCount)

	// The warning names the passkey by its short reference, never by its id.
	sum := sha256.Sum256(p.cred.ID)
	ref := hex.EncodeToString(sum[:])[:8]
	var warns []string
	for _, line := range strings.Split(p.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["level"] == "warn" && m["passkey"] == ref {
			warns = append(warns, line)
		}
	}
	assert.Len(t, warns, 2, "one warning per refused clone")
	assert.NotContains(t, p.logs.String(), base64.RawURLEncoding.EncodeToString(p.cred.ID))
}
