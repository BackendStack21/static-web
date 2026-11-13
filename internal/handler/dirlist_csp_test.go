package handler

import (
	"strings"
	"testing"
)

// HTTP-F7: the listing's CSP is the configured policy plus the hash of its
// inline <style>, with every other restriction kept.
func TestListingCSP_AddsStyleHashAndKeepsRestrictions(t *testing.T) {
	h := dirListStyleSource
	if !strings.HasPrefix(h, "'sha256-") || !strings.HasSuffix(h, "='") {
		t.Fatalf("dirListStyleSource = %q, want a quoted sha256 hash source", h)
	}
	tests := []struct {
		name, in, want string
	}{
		{"empty policy stays empty", "", ""},
		{"default-src only: style-src derived from default-src",
			"default-src 'self'",
			"default-src 'self'; style-src 'self' " + h},
		{"default-src sources are all copied",
			"default-src 'self' https://cdn.example; img-src data:",
			"default-src 'self' https://cdn.example; img-src data:; style-src 'self' https://cdn.example " + h},
		{"default-src 'none' becomes the hash alone",
			"default-src 'none'; img-src 'self'",
			"default-src 'none'; img-src 'self'; style-src " + h},
		{"existing style-src gets the hash",
			"default-src 'self'; style-src 'self' https://fonts.example",
			"default-src 'self'; style-src 'self' https://fonts.example " + h},
		{"style-src 'none' becomes the hash alone",
			"default-src 'self'; style-src 'none'",
			"default-src 'self'; style-src " + h},
		{"style-src-elem gets the hash and style-src is derived",
			"default-src 'self'; style-src-elem 'self'",
			"default-src 'self'; style-src-elem 'self' " + h + "; style-src 'self' " + h},
		{"style-src and style-src-elem both get the hash",
			"style-src 'self'; style-src-elem 'none'",
			"style-src 'self' " + h + "; style-src-elem " + h},
		{"no default-src and no style-src: styles already allowed, unchanged",
			"img-src 'self';  frame-ancestors 'none'",
			"img-src 'self';  frame-ancestors 'none'"},
		{"directive names are case-insensitive",
			"Default-Src 'self'",
			"Default-Src 'self'; style-src 'self' " + h},
		{"only the first of duplicate directives counts",
			"default-src 'self'; style-src 'self'; style-src 'none'",
			"default-src 'self'; style-src 'self' " + h + "; style-src 'none'"},
		{"trailing semicolon and extra spaces",
			"  default-src   'self' ;",
			"default-src 'self'; style-src 'self' " + h},
		{"report directives are kept",
			"default-src 'self'; report-uri /csp",
			"default-src 'self'; report-uri /csp; style-src 'self' " + h},
		{"each comma-separated policy is rewritten",
			"default-src 'self', img-src 'none'",
			"default-src 'self'; style-src 'self' " + h + ", img-src 'none'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listingCSP(tt.in); got != tt.want {
				t.Errorf("listingCSP(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}
