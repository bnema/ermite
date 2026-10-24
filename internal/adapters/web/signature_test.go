// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

const (
	sshFP = "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	gpgFP = "0123456789ABCDEF0123456789ABCDEF01234567"
)

func verification(f domain.SignatureFormat, s domain.SignatureStatus, fp, title string) domain.CommitVerification {
	return domain.CommitVerification{Format: f, Status: s, Fingerprint: fp, KeyTitle: title}
}

func TestSignatureBadge(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		v            domain.CommitVerification
		long         bool
		nilBadge     bool
		proto, label string
		class, title string
	}{
		{name: "unchecked", v: domain.CommitVerification{}, nilBadge: true},
		{name: "unchecked long", v: domain.CommitVerification{}, long: true, nilBadge: true},
		{name: "unsigned in a list", v: verification("", domain.StatusUnsigned, "", ""), nilBadge: true},
		{name: "unsigned on the page", v: verification("", domain.StatusUnsigned, "", ""), long: true, label: "unsigned", class: "sig-none"},
		{name: "verified ssh", v: verification(domain.FormatSSH, domain.StatusVerified, sshFP, "laptop"),
			proto: "SSH", label: "Verified", class: "sig-ok", title: "Signed with SSH key " + sshFP + " (laptop) — verified for the committer email"},
		{name: "verified gpg", v: verification(domain.FormatGPG, domain.StatusVerified, gpgFP, ""),
			proto: "GPG", label: "Verified", class: "sig-ok", title: "Signed with GPG key " + gpgFP + " — verified for the committer email"},
		{name: "unknown key", v: verification(domain.FormatGPG, domain.StatusUnknownKey, gpgFP, ""),
			proto: "GPG", label: "Unverified", class: "sig-warn", title: "Signed with an unknown GPG key " + gpgFP},
		{name: "email mismatch", v: verification(domain.FormatSSH, domain.StatusEmailMismatch, sshFP, "laptop"),
			proto: "SSH", label: "Unverified", class: "sig-warn", title: "this key is not trusted for the committer email"},
		{name: "expired", v: verification(domain.FormatGPG, domain.StatusExpired, gpgFP, ""),
			proto: "GPG", label: "Unverified", class: "sig-warn", title: "expired"},
		{name: "revoked", v: verification(domain.FormatGPG, domain.StatusRevoked, gpgFP, ""),
			proto: "GPG", label: "Unverified", class: "sig-warn", title: "revoked"},
		{name: "bad signature", v: verification(domain.FormatSSH, domain.StatusBadSignature, "", ""),
			proto: "SSH", label: "Bad signature", class: "sig-bad", title: "The signature does not match the commit"},
		{name: "x509", v: verification(domain.FormatX509, domain.StatusUnsupported, "", ""),
			proto: "X.509", label: "Unverified", class: "sig-warn", title: "X.509 signatures are not checked"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := signatureBadge(tt.v, tt.long)
			if tt.nilBadge {
				assert.Nil(t, b)
				return
			}
			require.NotNil(t, b)
			assert.Equal(t, tt.proto, b.Proto)
			assert.Equal(t, tt.label, b.Label)
			assert.Equal(t, tt.class, b.Class)
			assert.Contains(t, b.Title, tt.title)
		})
	}
}

func rowOf(v domain.CommitVerification) CommitRow {
	id := objectID(&testing.T{}, "0123456789abcdef0123456789abcdef01234567")
	return commitRow("demo", domain.Commit{
		ID: id, Summary: "s", Verification: v,
		Author: domain.Signature{Name: "A"}, Committer: domain.Signature{Time: time.Unix(1, 0)},
	}, time.Unix(100, 0))
}

func TestCommitRowsShowTheSignatureBadge(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		v    domain.CommitVerification
		want []string
		none bool
	}{
		{name: "ssh verified", v: verification(domain.FormatSSH, domain.StatusVerified, sshFP, "laptop"),
			want: []string{`class="sig-ico sig-ok"`, `class="ico i-shield-check"`, `<span class="sr">SSH Verified</span>`, `title="Signed with SSH key ` + sshFP}},
		{name: "gpg verified", v: verification(domain.FormatGPG, domain.StatusVerified, gpgFP, ""),
			want: []string{`class="sig-ico sig-ok"`, `<span class="sr">GPG Verified</span>`}},
		{name: "unknown key", v: verification(domain.FormatGPG, domain.StatusUnknownKey, gpgFP, ""),
			want: []string{`class="sig-ico sig-warn"`, `class="ico i-shield-alert"`, `<span class="sr">GPG Unverified</span>`, `unknown GPG key`}},
		{name: "bad signature", v: verification(domain.FormatSSH, domain.StatusBadSignature, "", ""),
			want: []string{`class="sig-ico sig-bad"`, `class="ico i-shield-x"`, `<span class="sr">SSH Bad signature</span>`}},
		{name: "unsigned", v: verification("", domain.StatusUnsigned, "", ""), none: true},
		{name: "unchecked", v: domain.CommitVerification{}, none: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := renderToString(t, CommitRows([]CommitRow{rowOf(tt.v)}))
			if tt.none {
				assert.NotContains(t, got, "sig")
				assert.NotContains(t, got, "unsigned")
				return
			}
			for _, w := range tt.want {
				assert.Contains(t, got, w)
			}
		})
	}
}

func TestCommitPageShowsTheSignature(t *testing.T) {
	t.Parallel()
	page := func(v domain.CommitVerification) string {
		return renderToString(t, Commit(Layout{Repo: "demo"}, CommitPage{
			SHA: "0123456789abcdef0123456789abcdef01234567", Short: "0123456", Subject: "s",
			Sig: signatureBadge(v, true), Verification: v,
		}))
	}
	got := page(verification(domain.FormatSSH, domain.StatusVerified, sshFP, "my <laptop>"))
	assert.Contains(t, got, "<dt>signature</dt>")
	assert.Contains(t, got, `<span class="sig-proto">SSH</span> Verified`)
	assert.Contains(t, got, "<code>"+sshFP+"</code>")
	assert.Contains(t, got, "my &lt;laptop&gt;")

	got = page(verification(domain.FormatGPG, domain.StatusUnknownKey, gpgFP, ""))
	assert.Contains(t, got, `sig-warn`)
	assert.Contains(t, got, "<code>"+gpgFP+"</code>")
	assert.Contains(t, got, `<span class="sig-why dim">The key is unknown</span>`, "the reason is text, not only a tooltip")
	assert.Equal(t, 1, strings.Count(got, ">"+gpgFP), "the fingerprint is shown once; the pill tooltip may repeat it")

	got = page(verification("", domain.StatusUnsigned, "", ""))
	assert.Contains(t, got, "<dt>signature</dt>")
	assert.Contains(t, got, "sig-none")
	assert.Contains(t, got, "unsigned")
	assert.NotContains(t, got, "<code>")
	assert.NotContains(t, got, "sig-why")

	got = page(domain.CommitVerification{})
	assert.NotContains(t, got, "signature")
}

func TestMergeRequestCommitsShowTheSignatureBadge(t *testing.T) {
	t.Parallel()
	signed := signatureBadge(verification(domain.FormatGPG, domain.StatusVerified, gpgFP, ""), false)
	got := renderToString(t, mrFooter(MRPage{Commits: []MRCommit{
		{Short: "aaaaaaa", Href: "/r/commit/a", Subject: "signed", Sig: signed},
		{Short: "bbbbbbb", Href: "/r/commit/b", Subject: "unsigned"},
	}}))
	assert.Equal(t, 1, strings.Count(got, `class="sig-ico sig-ok"`), "only the signed commit has a shield")
	assert.Contains(t, got, `<span class="sr">GPG Verified</span>`)
}
