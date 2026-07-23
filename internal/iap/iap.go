// Package iap adds Google Cloud Identity-Aware Proxy (IAP) credentials to
// outbound requests. When the ArgoCD instance sits behind IAP, every
// server-to-server call (spec fetch, Dex token exchange, ArgoCD API calls)
// must present a Google-signed OIDC token or IAP rejects it before the
// request reaches ArgoCD.
//
// The token is sent in the Proxy-Authorization header rather than
// Authorization: IAP authorizes the request with the Proxy-Authorization
// token and forwards the Authorization header (the ArgoCD/Dex bearer token)
// to the backend untouched.
package iap

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

// Injector attaches a Google IAP OIDC identity token to outbound requests.
// A nil *Injector is valid and disables injection, so callers can always call
// Wrap without a nil check.
type Injector struct {
	ts oauth2.TokenSource
}

// New builds an Injector for the given IAP audience. The audience is the value
// IAP expects in the token's aud claim — typically the IAP OAuth 2.0 client ID
// (ending in .apps.googleusercontent.com), or the IAP-secured resource URL,
// depending on the IAP configuration.
//
// Credentials are sourced from Application Default Credentials (the
// GOOGLE_APPLICATION_CREDENTIALS service-account key, workload identity, or the
// GCE metadata server).
//
// Returns (nil, nil) when audience is empty, meaning IAP injection is disabled.
func New(ctx context.Context, audience string) (*Injector, error) {
	if audience == "" {
		return nil, nil
	}
	ts, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return nil, fmt.Errorf("create IAP token source (audience %q): %w", audience, err)
	}
	return &Injector{ts: ts}, nil
}

// Wrap returns a RoundTripper that adds the IAP Proxy-Authorization header to
// each request before delegating to base. When inj is nil, base is returned
// unchanged. A nil base is treated as http.DefaultTransport.
func (inj *Injector) Wrap(base http.RoundTripper) http.RoundTripper {
	if inj == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, ts: inj.ts}
}

type transport struct {
	base http.RoundTripper
	ts   oauth2.TokenSource
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.ts.Token()
	if err != nil {
		return nil, fmt.Errorf("mint IAP token: %w", err)
	}
	// The RoundTripper contract forbids mutating the caller's request, so clone
	// before setting the header. The OIDC ID token JWT is carried in
	// Token.AccessToken by the idtoken source.
	clone := req.Clone(req.Context())
	clone.Header.Set("Proxy-Authorization", "Bearer "+tok.AccessToken)
	return t.base.RoundTrip(clone)
}
