// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

// TrustedActor reports whether a run started by p is trusted: its jobs get
// secrets and, later, the write permissions of their workflow (CI-3, D1).
//
// Only the owner is trusted: the root token, the owner signed in on the web, and
// a stored token with the owner's rights (every scope, no repository list, no
// branch prefixes: Principal.HasOwnerRights). Everything else is not: a token
// limited to branch prefixes (an automated agent could otherwise push a
// workflow that prints any secret), a token limited to some repositories or
// scopes, a job token and an anonymous caller. When in doubt the answer is no:
// an untrusted run only misses its secrets, a trusted one by mistake leaks them.
func TrustedActor(p Principal) bool { return p.HasOwnerRights() }

// RerunTrusted is the trusted flag of a new attempt of a run: the run was
// trusted and the caller of the re-run is trusted too. A re-run keeps the flag
// or lowers it and never raises it: the owner who runs an agent's run again
// does not unlock its secrets.
func RerunTrusted(original bool, caller Principal) bool { return original && TrustedActor(caller) }
