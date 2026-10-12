// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webauthn implements ports.PasskeyVerifier over go-webauthn (ADR
// 0026). It is the only importer of the library: the use case sees plain
// domain values and opaque ceremony bytes.
package webauthn

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ownerName names the one user in the options sent to the browser.
const ownerName = "owner"

// Verifier runs the WebAuthn ceremonies for the owner of one public URL.
type Verifier struct {
	web *gowebauthn.WebAuthn
}

var _ ports.PasskeyVerifier = (*Verifier)(nil)

// New builds a verifier for publicURL: the relying party ID is its host name,
// the only accepted origin is its origin. It fails when the library refuses the
// configuration, for example for a host that is an IP address.
func New(publicURL string) (*Verifier, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, fmt.Errorf("passkeys: parse public URL: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if (u.Scheme != "http" && u.Scheme != "https") || host == "" {
		return nil, fmt.Errorf("passkeys: public URL %q needs an http(s) scheme and a host", publicURL)
	}
	timeout := gowebauthn.TimeoutConfig{Enforce: true, Timeout: domain.PasskeyCeremonyTTL, TimeoutUVD: domain.PasskeyCeremonyTTL}
	web, err := gowebauthn.New(&gowebauthn.Config{
		RPID:          host,
		RPDisplayName: host,
		RPOrigins:     []string{u.Scheme + "://" + strings.ToLower(u.Host)},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts:              gowebauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
	if err != nil {
		return nil, fmt.Errorf("passkeys: %w", err)
	}
	return &Verifier{web: web}, nil
}

// owner is the single WebAuthn user.
type owner struct {
	handle      []byte
	credentials []gowebauthn.Credential
}

func (o owner) WebAuthnID() []byte                           { return o.handle }
func (o owner) WebAuthnName() string                         { return ownerName }
func (o owner) WebAuthnDisplayName() string                  { return ownerName }
func (o owner) WebAuthnCredentials() []gowebauthn.Credential { return o.credentials }

// refuse builds the error of a failed verification: an ErrUnauthenticated whose
// text carries the reason, for logs. The library error is flattened to text so
// that its types never leave this package.
func refuse(what string, err error) error {
	return fmt.Errorf("%w: passkey %s: %v", domain.ErrUnauthenticated, what, err)
}

// BeginRegistration implements ports.PasskeyVerifier.
func (v *Verifier) BeginRegistration(userHandle []byte, existing []domain.Passkey) ([]byte, []byte, error) {
	exclude := make([]protocol.CredentialDescriptor, 0, len(existing))
	for _, p := range existing {
		exclude = append(exclude, descriptor(p))
	}
	creation, session, err := v.web.BeginRegistration(owner{handle: userHandle},
		gowebauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		gowebauthn.WithExclusions(exclude),
		gowebauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("begin passkey registration: %w", err)
	}
	return encode(creation, session)
}

// FinishRegistration implements ports.PasskeyVerifier.
func (v *Verifier) FinishRegistration(userHandle []byte, ceremony, response []byte) (domain.Passkey, error) {
	var session gowebauthn.SessionData
	if err := json.Unmarshal(ceremony, &session); err != nil {
		return domain.Passkey{}, refuse("registration", fmt.Errorf("decode ceremony: %w", err))
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return domain.Passkey{}, refuse("registration", err)
	}
	cred, err := v.web.CreateCredential(owner{handle: userHandle}, session, parsed)
	if err != nil {
		return domain.Passkey{}, refuse("registration", err)
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	return domain.Passkey{
		ID:              cred.ID,
		PublicKey:       cred.PublicKey,
		AttestationType: cred.AttestationType,
		Transports:      transports,
		SignCount:       cred.Authenticator.SignCount,
		BackupEligible:  cred.Flags.BackupEligible,
		BackupState:     cred.Flags.BackupState,
	}, nil
}

// BeginLogin implements ports.PasskeyVerifier.
func (v *Verifier) BeginLogin() ([]byte, []byte, error) {
	assertion, session, err := v.web.BeginDiscoverableLogin()
	if err != nil {
		return nil, nil, fmt.Errorf("begin passkey login: %w", err)
	}
	return encode(assertion, session)
}

// FinishLogin implements ports.PasskeyVerifier. The library compares the user
// handle of the response with userHandle.
//
//hexcheck:ignore suspicious-business-logic-in-adapter copies the counter and backup state the library verified into the stored passkey; it decides nothing: the use case refuses a counter that does not advance
func (v *Verifier) FinishLogin(userHandle []byte, ceremony, response []byte, lookup func(id []byte) (domain.Passkey, error)) (domain.Passkey, error) {
	var session gowebauthn.SessionData
	if err := json.Unmarshal(ceremony, &session); err != nil {
		return domain.Passkey{}, refuse("login", fmt.Errorf("decode ceremony: %w", err))
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return domain.Passkey{}, refuse("login", err)
	}

	var stored domain.Passkey
	var lookupErr error // a failure of the store, as opposed to an unknown credential
	handler := func(rawID, _ []byte) (gowebauthn.User, error) {
		p, err := lookup(rawID)
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) {
				lookupErr = err
			}
			return nil, err
		}
		stored = p
		return owner{handle: userHandle, credentials: []gowebauthn.Credential{credential(p)}}, nil
	}
	_, cred, err := v.web.ValidatePasskeyLogin(handler, session, parsed)
	if lookupErr != nil {
		return domain.Passkey{}, lookupErr // fail closed, as it is
	}
	if err != nil {
		return domain.Passkey{}, refuse("login", err)
	}
	// The library keeps the stored counter when the received one does not
	// advance (its clone warning); the use case refuses that, under its lock.
	stored.SignCount = cred.Authenticator.SignCount
	stored.BackupState = cred.Flags.BackupState
	return stored, nil
}

// encode returns the JSON of the options for the browser and of the session.
func encode(options any, session *gowebauthn.SessionData) (opts, ceremony []byte, err error) {
	if opts, err = json.Marshal(options); err != nil {
		return nil, nil, fmt.Errorf("encode passkey options: %w", err)
	}
	if ceremony, err = json.Marshal(session); err != nil {
		return nil, nil, fmt.Errorf("encode passkey ceremony: %w", err)
	}
	return opts, ceremony, nil
}

// credential converts a stored passkey into the library's form.
func credential(p domain.Passkey) gowebauthn.Credential {
	c := gowebauthn.Credential{
		ID:              p.ID,
		PublicKey:       p.PublicKey,
		AttestationType: p.AttestationType,
		Flags:           gowebauthn.CredentialFlags{BackupEligible: p.BackupEligible, BackupState: p.BackupState},
		Authenticator:   gowebauthn.Authenticator{SignCount: p.SignCount},
	}
	for _, t := range p.Transports {
		c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
	}
	return c
}

// descriptor names a stored passkey for the exclusion list.
func descriptor(p domain.Passkey) protocol.CredentialDescriptor {
	c := credential(p)
	return c.Descriptor()
}
