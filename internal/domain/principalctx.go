// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "context"

type principalKey struct{}

// ContextWithPrincipal returns a context that carries p as the identity of the
// caller. The API adapter sets it after authenticating a request; the web
// sets it for the calls it makes in process (anonymous, or the owner of a
// session). Authorization stays in the use cases.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the principal ContextWithPrincipal stored.
// ok is false when the context carries none.
func PrincipalFromContext(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(principalKey{}).(Principal)
	return p, ok
}
