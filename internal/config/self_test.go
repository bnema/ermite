// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrustedProxiesSelf(t *testing.T) {
	tests := []struct {
		name, raw string
		addresses []string
		sourceErr error
		want      []string
		wantErr   string
	}{
		{name: "host addresses", raw: "self", addresses: []string{"10.0.0.2/24", "fd00::2/64", "127.0.0.1/8", "::1/128", "::ffff:192.0.2.1/128"}, want: []string{"10.0.0.2/32", "fd00::2/128", "127.0.0.1/32", "::1/128", "192.0.2.1/32"}},
		{name: "mixed and deduplicated", raw: "self, 10.0.0.0/8, 10.0.0.2/32, self, 10.1.0.0/8", addresses: []string{"10.0.0.2/24", "10.0.0.2/32"}, want: []string{"10.0.0.2/32", "10.0.0.0/8"}},
		{name: "non unicast skipped", raw: "self", addresses: []string{"fe80::1/64", "169.254.1.1/16", "::ffff:169.254.1.1/128", "224.0.0.1/4", "::/128", "0.0.0.0/32", "127.0.0.1/8"}, want: []string{"127.0.0.1/32"}},
		{name: "empty", raw: "self", wantErr: "no eligible host addresses"},
		{name: "only link local with explicit CIDR", raw: "10.0.0.0/8,self", addresses: []string{"fe80::1/64"}, wantErr: "no eligible host addresses"},
		{name: "source failure", raw: "self", sourceErr: errors.New("enumeration failed"), wantErr: "enumeration failed"},
		{name: "case sensitive", raw: "Self", wantErr: "not self or a CIDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := interfaceAddrs
			t.Cleanup(func() { interfaceAddrs = original })
			interfaceAddrs = func() ([]net.Addr, error) {
				var addresses []net.Addr
				for _, raw := range tt.addresses {
					p := netip.MustParsePrefix(raw)
					addresses = append(addresses, &net.IPNet{IP: net.IP(p.Addr().AsSlice()), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())})
				}
				return addresses, tt.sourceErr
			}
			cfg, err := Load(func(key string) string {
				switch key {
				case EnvTrustedProxies:
					return tt.raw
				case EnvAdminToken:
					return "0123456789abcdef0123456789abcdef"
				case EnvDataDir:
					return t.TempDir()
				case EnvPublicURL:
					return "https://git.example.com"
				default:
					return ""
				}
			})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, EnvTrustedProxies)
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			var want []netip.Prefix
			for _, raw := range tt.want {
				want = append(want, netip.MustParsePrefix(raw))
			}
			require.Equal(t, want, cfg.TrustedProxies)
		})
	}
}
