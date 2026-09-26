package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/matthisholleville/argocd-mcp/internal/openapi"
)

// fakeArgoCD serves the four endpoints diff_application reads. The app's
// ConfigMap changes data.a from "1" to "2" at the requested revision.
func fakeArgoCD(t *testing.T, manifestsStatus int) (*Gateway, *[]string) {
	t.Helper()
	cm := func(value string) string {
		return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"demo-ns",` +
			`"annotations":{"argocd.argoproj.io/tracking-id":"demo:/ConfigMap:demo-ns/cfg"}},"data":{"a":"` + value + `"}}`
	}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api/v1/applications/demo":
			body = map[string]any{"metadata": map[string]any{"name": "demo"},
				"spec": map[string]any{"destination": map[string]any{"namespace": "demo-ns"}}}
		case "/api/v1/applications/demo/managed-resources":
			body = map[string]any{"items": []any{map[string]any{
				"kind": "ConfigMap", "namespace": "demo-ns", "name": "cfg",
				"liveState": cm("1"), "normalizedLiveState": cm("1"), "predictedLiveState": cm("1"), "targetState": cm("1"),
			}}}
		case "/api/v1/applications/demo/manifests":
			if manifestsStatus != http.StatusOK {
				w.WriteHeader(manifestsStatus)
				_, _ = w.Write([]byte(`{"error":"unable to resolve 'nope' to a commit SHA"}`))
				return
			}
			body = map[string]any{"manifests": []string{cm("2")}}
		case "/api/v1/settings":
			body = map[string]any{"appLabelKey": "argocd.argoproj.io/instance", "trackingMethod": "annotation"}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return NewGateway(srv.URL, "test-token", false, nil, slog.Default()), &paths
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	return r.Content[0].(mcp.TextContent).Text
}

func TestHandleDiff_ReturnsUnifiedDiff(t *testing.T) {
	gw, paths := fakeArgoCD(t, http.StatusOK)
	handler := handleDiff(gw, nil, noopLimiter, nil)

	result, err := handler(context.Background(), buildCallToolRequest(t, map[string]any{"app": "demo", "revision": "abc123"}))
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: %v %v", err, result)
	}
	text := resultText(t, result)
	for _, want := range []string{"# 1 resource(s) differ: 1 modified", "===== /ConfigMap demo-ns/cfg ======", `-  a: "1"`, `+  a: "2"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.Contains(strings.Join(*paths, "\n"), "/manifests?revision=abc123") {
		t.Errorf("revision not passed to manifests: %v", *paths)
	}
}

func TestHandleDiff_JSONFormat(t *testing.T) {
	gw, _ := fakeArgoCD(t, http.StatusOK)
	handler := handleDiff(gw, nil, noopLimiter, nil)

	result, err := handler(context.Background(), buildCallToolRequest(t, map[string]any{"app": "demo", "revision": "abc123", "format": "json"}))
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: %v %v", err, result)
	}
	var out struct {
		HasDiff   bool `json:"has_diff"`
		Resources []struct {
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), &out); err != nil {
		t.Fatal(err)
	}
	if !out.HasDiff || out.Resources[0].Changes[0].Path != "/data/a" {
		t.Errorf("unexpected JSON result: %+v", out)
	}
}

func TestHandleDiff_PropagatesAPIErrors(t *testing.T) {
	gw, _ := fakeArgoCD(t, http.StatusBadRequest)
	handler := handleDiff(gw, nil, noopLimiter, nil)

	result, err := handler(context.Background(), buildCallToolRequest(t, map[string]any{"app": "demo", "revision": "nope"}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "unable to resolve") {
		t.Errorf("expected API error to surface, got: %v", resultText(t, result))
	}
}

func TestHandleDiff_AllowedResourcesAndAudit(t *testing.T) {
	gw, paths := fakeArgoCD(t, http.StatusOK)
	// Only projects are allowed, so the applications calls are out of scope.
	allowed := openapi.NewAllowedEndpoints([]openapi.Endpoint{{Method: "GET", Path: "/api/v1/projects"}})
	var buf bytes.Buffer
	handler := handleDiff(gw, allowed, noopLimiter, newTestAuditor(&buf))

	result, err := handler(context.Background(), buildCallToolRequest(t, map[string]any{"app": "demo", "revision": "abc123"}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "ALLOWED_RESOURCES") {
		t.Errorf("expected ALLOWED_RESOURCES block, got: %s", resultText(t, result))
	}
	if len(*paths) != 0 {
		t.Errorf("no API calls should be made when blocked, got %v", *paths)
	}
	entries := parseAuditLines(t, &buf)
	if len(entries) != 1 || entries[0]["tool"] != "diff_application" || entries[0]["blocked"] != true {
		t.Errorf("audit entries: %v", entries)
	}
}
