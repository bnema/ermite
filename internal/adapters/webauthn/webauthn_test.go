// SPDX-License-Identifier: AGPL-3.0-or-later

package webauthn_test

import (
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/webauthn"
	"bnema.dev/ermite/internal/domain"
)

var userHandle = []byte("0123456789abcdef0123456789abcdef")

// site is one public URL and the relying party a browser would present for it.
type site struct {
	url string
	rp  virtualwebauthn.RelyingParty
}

var sites = map[string]site{
	"localhost": {"http://localhost:7341", virtualwebauthn.RelyingParty{ID: "localhost", Name: "localhost", Origin: "http://localhost:7341"}},
	"https":     {"https://git.example.test", virtualwebauthn.RelyingParty{ID: "git.example.test", Name: "git.example.test", Origin: "https://git.example.test"}},
}

// device is a virtual authenticator with one credential and the passkey the
// server stored for it.
type device struct {
	auth  virtualwebauthn.Authenticator
	cred  virtualwebauthn.Credential
	store map[string]domain.Passkey // by credential id
}

func (d *device) lookup(id []byte) (domain.Passkey, error) {
	p, ok := d.store[string(id)]
	if !ok {
		return domain.Passkey{}, domain.ErrNotFound
	}
	return p, nil
}

// register runs a registration ceremony against v with a new credential.
func register(t *testing.T, v *webauthn.Verifier, s site, opts virtualwebauthn.AuthenticatorOptions) *device {
	t.Helper()
	opts.UserHandle = userHandle
	d := &device{
		auth:  virtualwebauthn.NewAuthenticatorWithOptions(opts),
		cred:  virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
		store: map[string]domain.Passkey{},
	}
	options, ceremony, err := v.BeginRegistration(userHandle, nil)
	require.NoError(t, err)
	parsed, err := virtualwebauthn.ParseAttestationOptions(string(options))
	require.NoError(t, err)
	response := virtualwebauthn.CreateAttestationResponse(s.rp, d.auth, d.cred, *parsed)

	p, err := v.FinishRegistration(userHandle, ceremony, []byte(response))
	require.NoError(t, err)
	assert.Equal(t, d.cred.ID, p.ID)
	assert.NotEmpty(t, p.PublicKey)
	p.Name = "test"
	d.store[string(p.ID)] = p
	d.auth.AddCredential(d.cred)
	return d
}

// assertion starts a login and returns the ceremony and the signed response.
func (d *device) assertion(t *testing.T, v *webauthn.Verifier, s site) (ceremony []byte, options *virtualwebauthn.AssertionOptions) {
	t.Helper()
	raw, ceremony, err := v.BeginLogin()
	require.NoError(t, err)
	options, err = virtualwebauthn.ParseAssertionOptions(string(raw))
	require.NoError(t, err)
	return ceremony, options
}

func (d *device) respond(s site, options *virtualwebauthn.AssertionOptions) []byte {
	return []byte(virtualwebauthn.CreateAssertionResponse(s.rp, d.auth, d.cred, *options))
}

func TestRegisterThenLogin(t *testing.T) {
	t.Parallel()
	for name, s := range sites {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v, err := webauthn.New(s.url)
			require.NoError(t, err)
			d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{BackupEligible: true})
			d.cred.Counter = 5

			ceremony, options := d.assertion(t, v, s)
			got, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
			require.NoError(t, err)
			assert.Equal(t, d.cred.ID, got.ID)
			assert.Equal(t, "test", got.Name, "the stored fields are kept")
			assert.Equal(t, uint32(5), got.SignCount, "the new sign count")
			assert.True(t, got.BackupEligible)
			assert.False(t, got.BackupState)
			assert.Equal(t, d.store[string(got.ID)].PublicKey, got.PublicKey)

			// The next login advances the counter again.
			d.store[string(got.ID)] = got
			d.cred.Counter = 6
			ceremony, options = d.assertion(t, v, s)
			got, err = v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
			require.NoError(t, err)
			assert.Equal(t, uint32(6), got.SignCount)
		})
	}
}

func TestRegistrationReportsBackupFlags(t *testing.T) {
	t.Parallel()
	s := sites["https"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)
	d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true,
		Transports: []virtualwebauthn.Transport{virtualwebauthn.TransportInternal, virtualwebauthn.TransportHybrid}})
	p := d.store[string(d.cred.ID)]
	assert.True(t, p.BackupEligible)
	assert.True(t, p.BackupState)
	assert.Equal(t, []string{"internal", "hybrid"}, p.Transports)

	// A login reports the backup state the authenticator has now.
	d.auth.Options.BackupState = false
	ceremony, options := d.assertion(t, v, s)
	got, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
	require.NoError(t, err)
	assert.False(t, got.BackupState)
}

func TestOptions(t *testing.T) {
	t.Parallel()
	s := sites["https"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)

	existing := domain.Passkey{ID: []byte("already-registered"), PublicKey: []byte("k"), Transports: []string{"usb"}}
	raw, _, err := v.BeginRegistration(userHandle, []domain.Passkey{existing})
	require.NoError(t, err)
	var creation struct {
		PublicKey struct {
			RP                     struct{ ID, Name string }
			User                   struct{ ID, Name string }
			Challenge              string
			AuthenticatorSelection struct {
				ResidentKey        string
				RequireResidentKey bool
				UserVerification   string
			}
			Attestation        string
			Timeout            int
			ExcludeCredentials []struct{ ID string }
		}
	}
	require.NoError(t, json.Unmarshal(raw, &creation))
	pk := creation.PublicKey
	assert.Equal(t, "git.example.test", pk.RP.ID)
	assert.Equal(t, "owner", pk.User.Name)
	assert.NotEmpty(t, pk.Challenge)
	assert.Equal(t, "required", pk.AuthenticatorSelection.ResidentKey)
	assert.True(t, pk.AuthenticatorSelection.RequireResidentKey)
	assert.Equal(t, "required", pk.AuthenticatorSelection.UserVerification)
	assert.Equal(t, "none", pk.Attestation)
	assert.Equal(t, 120000, pk.Timeout)
	require.Len(t, pk.ExcludeCredentials, 1)
	parsed, err := virtualwebauthn.ParseAttestationOptions(string(raw))
	require.NoError(t, err)
	require.Len(t, parsed.ExcludeCredentials, 1)

	raw, _, err = v.BeginLogin()
	require.NoError(t, err)
	var assertion struct {
		PublicKey struct {
			RPID             string
			Challenge        string
			UserVerification string
			Timeout          int
			AllowCredentials []struct{ ID string }
		}
	}
	require.NoError(t, json.Unmarshal(raw, &assertion))
	assert.Equal(t, "git.example.test", assertion.PublicKey.RPID)
	assert.NotEmpty(t, assertion.PublicKey.Challenge)
	assert.Equal(t, "required", assertion.PublicKey.UserVerification, "login inherits user verification")
	assert.Equal(t, 120000, assertion.PublicKey.Timeout)
	assert.Empty(t, assertion.PublicKey.AllowCredentials, "a discoverable login lists no credential")
}

func TestLoginRefused(t *testing.T) {
	t.Parallel()
	s := sites["localhost"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)

	tests := []struct {
		name string
		// run returns the error of FinishLogin.
		run func(t *testing.T, d *device) error
	}{
		{"wrong origin", func(t *testing.T, d *device) error {
			ceremony, options := d.assertion(t, v, s)
			other := s
			other.rp.Origin = "http://localhost:9999"
			return finish(v, ceremony, d.respond(other, options), d)
		}},
		{"wrong RP ID", func(t *testing.T, d *device) error {
			ceremony, options := d.assertion(t, v, s)
			other := s
			other.rp.ID = "evil.example.test"
			return finish(v, ceremony, d.respond(other, options), d)
		}},
		{"tampered challenge", func(t *testing.T, d *device) error {
			ceremony, options := d.assertion(t, v, s)
			options.Challenge[0] ^= 0xff
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"challenge of another ceremony", func(t *testing.T, d *device) error {
			ceremony, _ := d.assertion(t, v, s)
			_, other := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, other), d)
		}},
		{"expired ceremony", func(t *testing.T, d *device) error {
			ceremony, options := d.assertion(t, v, s)
			return finish(v, setExpires(t, ceremony, time.Now().Add(-time.Second)), d.respond(s, options), d)
		}},
		{"garbage ceremony", func(t *testing.T, d *device) error {
			_, options := d.assertion(t, v, s)
			return finish(v, []byte("not json"), d.respond(s, options), d)
		}},
		{"garbage response", func(t *testing.T, d *device) error {
			ceremony, _ := d.assertion(t, v, s)
			return finish(v, ceremony, []byte("{}"), d)
		}},
		{"user not verified", func(t *testing.T, d *device) error {
			d.auth.Options.UserNotVerified = true
			ceremony, options := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"user not present", func(t *testing.T, d *device) error {
			d.auth.Options.UserNotPresent = true
			ceremony, options := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"unknown credential", func(t *testing.T, d *device) error {
			ceremony, options := d.assertion(t, v, s)
			delete(d.store, string(d.cred.ID))
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"other user handle", func(t *testing.T, d *device) error {
			d.auth.Options.UserHandle = []byte("a-handle-of-somebody-else-------")
			ceremony, options := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"no user handle", func(t *testing.T, d *device) error {
			d.auth.Options.UserHandle = nil
			ceremony, options := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, options), d)
		}},
		{"registration ceremony used for a login", func(t *testing.T, d *device) error {
			_, ceremony, err := v.BeginRegistration(userHandle, nil)
			require.NoError(t, err)
			_, options := d.assertion(t, v, s)
			return finish(v, ceremony, d.respond(s, options), d)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{})
			opts := d.auth.Options
			stored := maps.Clone(d.store)
			err := tc.run(t, d)
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)

			// Control: the refusal was for the reason of the case. With the device
			// and the store as they were, a fresh assertion still succeeds, so the
			// case did not fail by something that breaks every login.
			d.auth.Options = opts
			d.store = stored
			ceremony, options := d.assertion(t, v, s)
			got, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
			require.NoError(t, err, "a fresh, untouched assertion must succeed")
			assert.Equal(t, d.cred.ID, got.ID)
		})
	}
}

func finish(v *webauthn.Verifier, ceremony, response []byte, d *device) error {
	_, err := v.FinishLogin(userHandle, ceremony, response, d.lookup)
	return err
}

// setExpires rewrites the expiry of a ceremony the way a late finish would see
// it, since the library checks it against the wall clock.
func setExpires(t *testing.T, ceremony []byte, at time.Time) []byte {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(ceremony, &m))
	require.Contains(t, m, "expires")
	m["expires"] = at
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}

func TestLoginStoreFailureIsNotAnUnauthenticated(t *testing.T) {
	t.Parallel()
	s := sites["localhost"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)
	d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{})
	ceremony, options := d.assertion(t, v, s)
	boom := errors.New("disk on fire")
	_, err = v.FinishLogin(userHandle, ceremony, d.respond(s, options), func([]byte) (domain.Passkey, error) {
		return domain.Passkey{}, boom
	})
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, domain.ErrUnauthenticated)
}

// A counter that does not advance is not refused here: the verifier reports the
// stored counter (the library keeps it on a clone warning) and the use case,
// which orders the logins under its lock, refuses it. TestPasskeyClonedAuthenticator
// in package app checks the refusal end to end.
func TestCounterThatDoesNotAdvanceKeepsTheStoredOne(t *testing.T) {
	t.Parallel()
	s := sites["localhost"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)
	d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{})

	d.cred.Counter = 1
	ceremony, options := d.assertion(t, v, s)
	got, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), got.SignCount)
	d.store[string(got.ID)] = got

	// The counter did not advance: a copy of the key may be in use. The answer
	// is valid, and carries the stored counter, not a lower or equal new one.
	ceremony, options = d.assertion(t, v, s)
	again, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), again.SignCount, "the stored counter, unchanged")

	// A response with a lower counter keeps the stored one too.
	d.cred.Counter = 0
	ceremony, options = d.assertion(t, v, s)
	lower, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), lower.SignCount)
}

// Synced passkeys always report 0: that is not a clone.
func TestZeroCounterIsNotAClone(t *testing.T) {
	t.Parallel()
	s := sites["localhost"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)
	d := register(t, v, s, virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true})
	for range 2 {
		ceremony, options := d.assertion(t, v, s)
		got, err := v.FinishLogin(userHandle, ceremony, d.respond(s, options), d.lookup)
		require.NoError(t, err)
		assert.Zero(t, got.SignCount)
	}
}

func TestRegistrationRefused(t *testing.T) {
	t.Parallel()
	s := sites["https"]
	v, err := webauthn.New(s.url)
	require.NoError(t, err)

	tests := []struct {
		name string
		auth virtualwebauthn.AuthenticatorOptions
		rp   func(rp *virtualwebauthn.RelyingParty)
		// ceremony replaces the ceremony data when set.
		ceremony func(t *testing.T, ceremony []byte) []byte
		handle   []byte
	}{
		{name: "wrong origin", rp: func(rp *virtualwebauthn.RelyingParty) { rp.Origin = "https://evil.example.test" }},
		{name: "wrong RP ID", rp: func(rp *virtualwebauthn.RelyingParty) { rp.ID = "evil.example.test" }},
		{name: "user not verified", auth: virtualwebauthn.AuthenticatorOptions{UserNotVerified: true}},
		{name: "expired ceremony", ceremony: func(t *testing.T, c []byte) []byte { return setExpires(t, c, time.Now().Add(-time.Second)) }},
		{name: "other user", handle: []byte("somebody-else-------------------")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rp := s.rp
			if tc.rp != nil {
				tc.rp(&rp)
			}
			auth := virtualwebauthn.NewAuthenticatorWithOptions(tc.auth)
			cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
			options, ceremony, err := v.BeginRegistration(userHandle, nil)
			require.NoError(t, err)
			parsed, err := virtualwebauthn.ParseAttestationOptions(string(options))
			require.NoError(t, err)
			response := virtualwebauthn.CreateAttestationResponse(rp, auth, cred, *parsed)
			if tc.ceremony != nil {
				ceremony = tc.ceremony(t, ceremony)
			}
			handle := userHandle
			if tc.handle != nil {
				handle = tc.handle
			}
			_, err = v.FinishRegistration(handle, ceremony, []byte(response))
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		})
	}

	t.Run("garbage", func(t *testing.T) {
		t.Parallel()
		_, ceremony, err := v.BeginRegistration(userHandle, nil)
		require.NoError(t, err)
		_, err = v.FinishRegistration(userHandle, ceremony, []byte("{"))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		_, err = v.FinishRegistration(userHandle, []byte("{"), []byte("{}"))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})
}

func TestNew(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"https://git.example.com", "http://localhost:7341", "https://Git.Example.com:8443/"} {
		_, err := webauthn.New(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"http://127.0.0.1:7341", "http://[::1]:7341", "git.example.com", "ftp://git.example.com", "", "https://"} {
		_, err := webauthn.New(bad)
		assert.Error(t, err, bad)
	}
}
