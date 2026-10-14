// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// CISettings is the use cases of secrets, variables and environments (ADR 0019,
// CI-3): the owner side served by the API, and the resolution of what a job
// sees.
//
// Secrets are write-only: no method returns a value but Resolve, which gives
// them to the planner for the job of a trusted run. Values are sealed with the
// sealer before they are stored; without one (no ERMITE_SECRETS_KEY) secrets
// cannot be set and jobs get none, while names can still be listed and deleted.
// The values are never logged.
type CISettings struct {
	meta   ports.RepoMetaStore
	store  ports.CISettingsStore
	sealer ports.SecretSealer // nil: no key
	clock  ports.Clock
}

var (
	_ ports.SecretService      = (*CISettings)(nil)
	_ ports.VariableService    = (*CISettings)(nil)
	_ ports.EnvironmentService = (*CISettings)(nil)
	_ ports.CIJobSettings      = (*CISettings)(nil)
)

// NewCISettings builds the settings use cases. sealer is nil when the server
// has no key. When there is one, it records the key check beside the secrets or
// compares it with the recorded one: another key than the one the secrets were
// sealed with is domain.ErrSecretsKeyMismatch, and the server must not start.
func NewCISettings(ctx context.Context, meta ports.RepoMetaStore, store ports.CISettingsStore, sealer ports.SecretSealer, clock ports.Clock) (*CISettings, error) {
	log := zerowrap.FromCtx(ctx)
	if sealer == nil {
		log.Warn().Msg("ERMITE_SECRETS_KEY is not set: secrets cannot be set and jobs receive none")
	} else if err := store.BindSecretsKey(ctx, sealer.KeyCheck()); err != nil {
		return nil, err
	}
	return &CISettings{meta: meta, store: store, sealer: sealer, clock: clock}, nil
}

// settingsAccess is the access rule of every call: secrets:write, never a token
// limited to branch prefixes (it never holds the owner's rights, ADR 0027; with
// secrets it could read back any value by pushing a workflow). The instance
// scope also needs a token that is not restricted to repositories. A repository
// that does not exist or that the caller cannot read is not found.
func (s *CISettings) settingsAccess(ctx context.Context, p domain.Principal, scope domain.SettingsScope) error {
	if !scope.Valid() {
		return &domain.InvalidFieldsError{Fields: []domain.FieldViolation{{Field: "scope", Reason: "is required and must name the instance, a repository or one of its environments"}}}
	}
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	if p.IsBranchLimited() {
		return fmt.Errorf("%w: a token limited to branch prefixes may not manage secrets and variables", domain.ErrForbidden)
	}
	if scope.Kind == domain.ScopeKindInstance {
		if !p.HasScope(domain.ScopeSecretsWrite) || len(p.Repos()) > 0 {
			return fmt.Errorf("%w: the instance scope needs the %s scope on a token that is not limited to some repositories", domain.ErrForbidden, domain.ScopeSecretsWrite)
		}
		return nil
	}
	return s.repoAccess(ctx, p, scope.Repo)
}

func (s *CISettings) repoAccess(ctx context.Context, p domain.Principal, name domain.RepoName) error {
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	if p.IsBranchLimited() {
		return fmt.Errorf("%w: a token limited to branch prefixes may not manage secrets and variables", domain.ErrForbidden)
	}
	repo, err := lookupRepo(ctx, s.meta, name)
	if err != nil {
		return err
	}
	if p.Allows(domain.ScopeSecretsWrite, name) {
		return nil
	}
	if !canRead(p, repo) {
		return domain.ErrNotFound
	}
	return fmt.Errorf("%w: this needs the %s scope on %s", domain.ErrForbidden, domain.ScopeSecretsWrite, name)
}

func logSetting(ctx context.Context, p domain.Principal, what string, scope domain.SettingsScope, name string) {
	ctxLog(ctx).Info().Str("scope", scope.Key()).Str("name", name).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg(what) // never the value
}

// SetSecret creates the secret or replaces its value.
func (s *CISettings) SetSecret(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name, value string) (domain.Secret, error) {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return domain.Secret{}, err
	}
	var v domain.Violations
	up, err := domain.ParseSettingName(name)
	if err != nil {
		v.AddErr("name", err)
	}
	if err := domain.CheckSecretValue(value); err != nil {
		v.AddErr("value", err)
	}
	if err := v.Err(); err != nil {
		return domain.Secret{}, err
	}
	if s.sealer == nil {
		return domain.Secret{}, fmt.Errorf("%w: the server has no ERMITE_SECRETS_KEY, so it cannot store secrets", domain.ErrPreconditionFailed)
	}
	sealed := s.sealer.Seal([]byte(value), domain.SecretAAD(scope, up))
	sec, err := s.store.PutSecret(ctx, scope, up, sealed, s.clock.Now().UTC())
	if err != nil {
		return domain.Secret{}, err
	}
	logSetting(ctx, p, "secret set", scope, up)
	return sec, nil
}

// ListSecrets lists the names of the secrets of one scope, ordered by name.
func (s *CISettings) ListSecrets(ctx context.Context, p domain.Principal, scope domain.SettingsScope, size int, token string) (ports.SecretPage, error) {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return ports.SecretPage{}, err
	}
	n, err := pageSize(size)
	if err != nil {
		return ports.SecretPage{}, err
	}
	after, err := decodeNameCursor(token, secretPageTokenPrefix)
	if err != nil {
		return ports.SecretPage{}, err
	}
	sealed, err := s.store.ListSealedSecrets(ctx, scope)
	if err != nil {
		return ports.SecretPage{}, err
	}
	all := make([]domain.Secret, 0, len(sealed))
	for _, x := range sealed {
		all = append(all, domain.Secret{Name: x.Name, UpdatedAt: x.UpdatedAt}) // the sealed value stops here
	}
	slices.SortFunc(all, func(a, b domain.Secret) int { return strings.Compare(a.Name, b.Name) })
	var page ports.SecretPage
	page.Secrets, page.NextPageToken = pageAfter(all, n,
		func(x domain.Secret) bool { return after == "" || x.Name > after },
		func(x domain.Secret) string { return encodeNameCursor(secretPageTokenPrefix, x.Name) })
	return page, nil
}

// DeleteSecret deletes a secret.
func (s *CISettings) DeleteSecret(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) error {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return err
	}
	up, err := domain.ParseSettingName(name)
	if err != nil {
		return notFoundOrInvalid(err)
	}
	if err := s.store.DeleteSecret(ctx, scope, up); err != nil {
		return err
	}
	logSetting(ctx, p, "secret deleted", scope, up)
	return nil
}

// notFoundOrInvalid maps a name that cannot exist to not found on the calls
// that name an existing thing (delete, get): a name that breaks the rules is a
// bad request on a write, but nothing is stored under it.
func notFoundOrInvalid(err error) error {
	var v domain.Violations
	v.AddErr("name", err)
	return v.Err()
}

// SetVariable creates the variable or replaces its value.
func (s *CISettings) SetVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name, value string) (domain.Variable, error) {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return domain.Variable{}, err
	}
	var v domain.Violations
	up, err := domain.ParseSettingName(name)
	if err != nil {
		v.AddErr("name", err)
	}
	if err := domain.CheckVariableValue(value); err != nil {
		v.AddErr("value", err)
	}
	if err := v.Err(); err != nil {
		return domain.Variable{}, err
	}
	out, err := s.store.PutVariable(ctx, scope, up, value, s.clock.Now().UTC())
	if err != nil {
		return domain.Variable{}, err
	}
	logSetting(ctx, p, "variable set", scope, up)
	return out, nil
}

// GetVariable returns one variable of one scope.
func (s *CISettings) GetVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) (domain.Variable, error) {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return domain.Variable{}, err
	}
	up, err := domain.ParseSettingName(name)
	if err != nil {
		return domain.Variable{}, notFoundOrInvalid(err)
	}
	return s.store.GetVariable(ctx, scope, up)
}

// ListVariables lists the variables of one scope, ordered by name.
func (s *CISettings) ListVariables(ctx context.Context, p domain.Principal, scope domain.SettingsScope, size int, token string) (ports.VariablePage, error) {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return ports.VariablePage{}, err
	}
	n, err := pageSize(size)
	if err != nil {
		return ports.VariablePage{}, err
	}
	after, err := decodeNameCursor(token, variablePageTokenPrefix)
	if err != nil {
		return ports.VariablePage{}, err
	}
	all, err := s.store.ListVariables(ctx, scope)
	if err != nil {
		return ports.VariablePage{}, err
	}
	slices.SortFunc(all, func(a, b domain.Variable) int { return strings.Compare(a.Name, b.Name) })
	var page ports.VariablePage
	page.Variables, page.NextPageToken = pageAfter(all, n,
		func(x domain.Variable) bool { return after == "" || x.Name > after },
		func(x domain.Variable) string { return encodeNameCursor(variablePageTokenPrefix, x.Name) })
	return page, nil
}

// DeleteVariable deletes a variable.
func (s *CISettings) DeleteVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) error {
	if err := s.settingsAccess(ctx, p, scope); err != nil {
		return err
	}
	up, err := domain.ParseSettingName(name)
	if err != nil {
		return notFoundOrInvalid(err)
	}
	if err := s.store.DeleteVariable(ctx, scope, up); err != nil {
		return err
	}
	logSetting(ctx, p, "variable deleted", scope, up)
	return nil
}

// CreateEnvironment creates an environment; one that exists (without regard to
// case) is returned unchanged.
func (s *CISettings) CreateEnvironment(ctx context.Context, p domain.Principal, repo domain.RepoName, name string) (domain.Environment, error) {
	if err := s.repoAccess(ctx, p, repo); err != nil {
		return domain.Environment{}, err
	}
	env, err := domain.ParseEnvironmentName(name)
	if err != nil {
		var v domain.Violations
		v.AddErr("name", err)
		return domain.Environment{}, v.Err()
	}
	out, err := s.store.CreateEnvironment(ctx, repo, env, s.clock.Now().UTC())
	if err != nil {
		return domain.Environment{}, err
	}
	logSetting(ctx, p, "environment created", domain.EnvironmentScope(repo, env), "")
	return out, nil
}

// ListEnvironments lists the environments of a repository, ordered by name.
func (s *CISettings) ListEnvironments(ctx context.Context, p domain.Principal, repo domain.RepoName, size int, token string) (ports.EnvironmentPage, error) {
	if err := s.repoAccess(ctx, p, repo); err != nil {
		return ports.EnvironmentPage{}, err
	}
	n, err := pageSize(size)
	if err != nil {
		return ports.EnvironmentPage{}, err
	}
	after, err := decodeNameCursor(token, environmentPageTokenPrefix)
	if err != nil {
		return ports.EnvironmentPage{}, err
	}
	all, err := s.store.ListEnvironments(ctx, repo)
	if err != nil {
		return ports.EnvironmentPage{}, err
	}
	key := func(e domain.Environment) string { return domain.EnvironmentKey(e.Name) }
	slices.SortFunc(all, func(a, b domain.Environment) int { return strings.Compare(key(a), key(b)) })
	var page ports.EnvironmentPage
	page.Environments, page.NextPageToken = pageAfter(all, n,
		func(x domain.Environment) bool { return after == "" || key(x) > after },
		func(x domain.Environment) string { return encodeNameCursor(environmentPageTokenPrefix, key(x)) })
	return page, nil
}

// DeleteEnvironment deletes an environment with its secrets and variables.
func (s *CISettings) DeleteEnvironment(ctx context.Context, p domain.Principal, repo domain.RepoName, name string) error {
	if err := s.repoAccess(ctx, p, repo); err != nil {
		return err
	}
	env, err := domain.ParseEnvironmentName(name)
	if err != nil {
		return domain.ErrNotFound
	}
	if err := s.store.DeleteEnvironment(ctx, repo, env); err != nil {
		return err
	}
	logSetting(ctx, p, "environment deleted", domain.EnvironmentScope(repo, env), "")
	return nil
}

// Page tokens name the last item of the previous page.
const (
	secretPageTokenPrefix      = "sn1:"
	variablePageTokenPrefix    = "vn1:"
	environmentPageTokenPrefix = "en1:"
)

func encodeNameCursor(prefix, last string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(prefix + last))
}

// decodeNameCursor returns "" for the empty token.
func decodeNameCursor(tok, prefix string) (string, error) {
	if tok == "" {
		return "", nil
	}
	raw, ok := decodeToken(tok, prefix)
	if !ok || raw == "" {
		return "", fmt.Errorf("%w: settings", domain.ErrInvalidPageToken)
	}
	return raw, nil
}

// Resolve gives the variables, and the secrets when wanted, that a job sees:
// the instance's, overridden by the repository's, overridden by the
// environment's. A job that names an environment creates it on first use.
// Secrets are opened only for the names asked (or all of them), and a secret
// that does not open is left out and logged by name, never by value.
func (s *CISettings) Resolve(ctx context.Context, req ports.JobSettingsRequest) (ports.JobSettings, error) {
	var out ports.JobSettings
	scopes := []domain.SettingsScope{domain.InstanceScope(), domain.RepoScope(req.Repo)}
	if req.Environment != "" {
		if req.CreateEnvironment {
			if _, err := s.store.CreateEnvironment(ctx, req.Repo, req.Environment, s.clock.Now().UTC()); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return out, err
			}
		}
		scopes = append(scopes, domain.EnvironmentScope(req.Repo, req.Environment))
	}
	out.Vars = map[string]string{}
	for _, scope := range scopes {
		vars, err := s.store.ListVariables(ctx, scope)
		if errors.Is(err, domain.ErrNotFound) {
			continue // no such repository or environment (yet): nothing to add
		}
		if err != nil {
			return ports.JobSettings{}, err
		}
		for _, v := range vars {
			out.Vars[v.Name] = v.Value
		}
	}
	if !req.WantSecrets || (!req.AllSecrets && len(req.SecretNames) == 0) {
		return out, nil
	}
	if s.sealer == nil {
		out.SecretsOff = true
		return out, nil
	}
	names := make([]string, 0, len(req.SecretNames))
	for _, n := range req.SecretNames {
		names = append(names, strings.ToUpper(n))
	}
	slices.Sort(names)
	names = slices.Compact(names)
	out.Secrets = map[string]string{}
	log := zerowrap.FromCtx(ctx)
	// Scopes go from the lowest precedence to the highest, so a later value
	// replaces an earlier one. A value that does not open is withheld, and
	// takes away what a lower scope gave: the override must never fall back to
	// the value it was meant to replace.
	admit := func(scope domain.SettingsScope, x ports.SealedSecret) {
		plain, err := s.sealer.Open(x.Sealed, domain.SecretAAD(scope, x.Name))
		if err != nil {
			delete(out.Secrets, x.Name)
			log.Error().Str("scope", scope.Key()).Str("name", x.Name).Msg("a stored secret does not open: it is left out of the job")
			return
		}
		out.Secrets[x.Name] = string(plain)
	}
	for _, scope := range scopes {
		if req.AllSecrets {
			sealed, err := s.store.ListSealedSecrets(ctx, scope)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return ports.JobSettings{}, err
			}
			for _, x := range sealed {
				admit(scope, x)
			}
			continue
		}
		for _, n := range names { // point reads: only the secrets the job names
			x, err := s.store.GetSealedSecret(ctx, scope, n)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return ports.JobSettings{}, err
			}
			admit(scope, x)
		}
	}
	return out, nil
}
