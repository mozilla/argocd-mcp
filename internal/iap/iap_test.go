package iap

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWrap_NilInjector_ReturnsBaseUnchanged(t *testing.T) {
	var inj *Injector // disabled
	base := http.DefaultTransport
	if got := inj.Wrap(base); got != base {
		t.Fatalf("nil injector should return base unchanged, got %v", got)
	}
}

func TestWrap_NilBase_UsesDefaultTransport(t *testing.T) {
	inj := &Injector{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"})}
	if got := inj.Wrap(nil); got == nil {
		t.Fatal("expected a non-nil wrapping transport when base is nil")
	}
}

func TestTransport_SetsProxyAuthAndPreservesAuthorization(t *testing.T) {
	inj := &Injector{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-id-token"})}

	var seenProxyAuth, seenAuth string
	capturing := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenProxyAuth = r.Header.Get("Proxy-Authorization")
		seenAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})

	rt := inj.Wrap(capturing)

	req := httptest.NewRequest(http.MethodGet, "https://argocd.example.com/api/v1/applications", nil)
	req.Header.Set("Authorization", "Bearer dex-token") // app's own auth must survive

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if want := "Bearer fake-id-token"; seenProxyAuth != want {
		t.Errorf("Proxy-Authorization = %q, want %q", seenProxyAuth, want)
	}
	if want := "Bearer dex-token"; seenAuth != want {
		t.Errorf("Authorization = %q, want %q (must be preserved for ArgoCD)", seenAuth, want)
	}
	// The caller's request must not be mutated (RoundTripper contract).
	if req.Header.Get("Proxy-Authorization") != "" {
		t.Error("original request was mutated with Proxy-Authorization header")
	}
}

func TestNew_EmptyAudience_ReturnsNilInjector(t *testing.T) {
	inj, err := New(t.Context(), "")
	if err != nil {
		t.Fatalf("New with empty audience: %v", err)
	}
	if inj != nil {
		t.Fatalf("expected nil injector for empty audience, got %v", inj)
	}
}
