package main

import "testing"

func TestNormalizeAuthMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    AuthMode
		wantErr bool
	}{
		{raw: "", want: AuthModeLocal},
		{raw: "local", want: AuthModeLocal},
		{raw: "  Local  ", want: AuthModeLocal},
		{raw: "none", want: AuthModeNone},
		{raw: "NONE", want: AuthModeNone},
		{raw: "trusted-header", wantErr: true},
		{raw: "oidc", wantErr: true},
		{raw: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeAuthMode(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeAuthMode(%q): expected an error, got mode %q", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeAuthMode(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeAuthMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{addr: ":8080", want: false},
		{addr: "0.0.0.0:8080", want: false},
		{addr: "127.0.0.1:8080", want: true},
		{addr: "127.5.5.5:8080", want: true},
		{addr: "localhost:8080", want: true},
		{addr: "LOCALHOST:8080", want: true},
		{addr: "[::1]:8080", want: true},
		{addr: "[::]:8080", want: false},
		{addr: "example.com:8080", want: false},
		{addr: "10.0.0.5:8080", want: false},
		{addr: "", want: false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
