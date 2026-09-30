package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/argoproj/argo-cd/v3/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/settings"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/matthisholleville/argocd-mcp/internal/appdiff"
	"github.com/matthisholleville/argocd-mcp/internal/audit"
	"github.com/matthisholleville/argocd-mcp/internal/openapi"
	"github.com/matthisholleville/argocd-mcp/internal/ratelimit"
)

// RegisterDiffTool registers the read-only diff_application tool.
func RegisterDiffTool(srv *server.MCPServer, gw *Gateway, allowed *openapi.AllowedEndpoints, limiter ratelimit.Limiter, auditor *audit.Logger) {
	srv.AddTool(diffTool(), handleDiff(gw, allowed, limiter, auditor))
}

func diffTool() mcp.Tool {
	return mcp.NewTool(
		"diff_application",
		mcp.WithDescription("Show what syncing an Argo CD Application to a git revision would change, "+
			"like `argocd app diff APP --revision REV`. Secrets and hooks are skipped, as in the CLI."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("app", mcp.Required(), mcp.Description("Application name")),
		mcp.WithString("revision", mcp.Required(), mcp.Description("Branch, tag, or commit SHA")),
		mcp.WithString("app_namespace", mcp.Description("Application namespace, if not the controller's")),
		mcp.WithString("mode", mcp.Enum(string(appdiff.ModeLive), string(appdiff.ModePR)),
			mcp.Description("live (default): what a sync would change, drift included. pr: only what the revision changes; drift is listed, not shown")),
		mcp.WithString("format", mcp.Enum("diff", "json"), mcp.Description("diff (default) or json with per-field changes")),
		mcp.WithBoolean("stat", mcp.Description("Changed resources only, no diff bodies")),
	)
}

func handleDiff(gw *Gateway, allowed *openapi.AllowedEndpoints, limiter ratelimit.Limiter, auditor *audit.Logger) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		app, err := req.RequireString("app")
		if err != nil {
			return mcp.NewToolResultError("app is required"), nil
		}
		revision, err := req.RequireString("revision")
		if err != nil {
			return mcp.NewToolResultError("revision is required"), nil
		}
		mode := appdiff.Mode(req.GetString("mode", string(appdiff.ModeLive)))
		if mode != appdiff.ModeLive && mode != appdiff.ModePR {
			return mcp.NewToolResultError("mode must be 'live' or 'pr'"), nil
		}
		format := req.GetString("format", "diff")
		if format != "diff" && format != "json" {
			return mcp.NewToolResultError("format must be 'diff' or 'json'"), nil
		}
		appNS := req.GetString("app_namespace", "")
		user := userFromContext(ctx)

		base := "/api/v1/applications/" + url.PathEscape(app)
		nsParams := map[string]string{}
		if appNS != "" {
			nsParams["appNamespace"] = appNS
		}
		manifestParams := map[string]string{"revision": revision}
		for k, v := range nsParams {
			manifestParams[k] = v
		}
		calls := []ExecuteParams{
			{Method: "GET", Path: base, QueryParams: nsParams},
			{Method: "GET", Path: base + "/managed-resources", QueryParams: nsParams},
			{Method: "GET", Path: base + "/manifests", QueryParams: manifestParams},
			{Method: "GET", Path: "/api/v1/settings"},
		}

		logEntry := func(e audit.Entry) {
			if auditor != nil {
				e.Tool, e.User, e.Method, e.Path, e.Query = "diff_application", user, "GET", base, "revision="+revision
				auditor.LogExecute(ctx, e)
			}
		}
		for _, c := range calls {
			if !allowed.IsAllowed(c.Method, c.Path) {
				logEntry(audit.Entry{Blocked: true})
				return mcp.NewToolResultError(fmt.Sprintf(
					"operation not allowed: GET %s is outside the permitted resource scope (ALLOWED_RESOURCES)", c.Path)), nil
			}
		}
		if !limiter.Allow(user) {
			logEntry(audit.Entry{Blocked: true})
			return mcp.NewToolResultError("rate limit exceeded: too many requests, please slow down"), nil
		}

		start := time.Now()
		result, err := runDiff(ctx, gw, calls, revision, mode)
		entry := audit.Entry{Duration: time.Since(start)}
		if err != nil {
			entry.Error = err.Error()
			logEntry(entry)
			return mcp.NewToolResultError(err.Error()), nil
		}
		entry.ResultCount = len(result.Resources)
		logEntry(entry)

		if format == "json" {
			data, err := result.JSON(req.GetBool("stat", false))
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
			}
			return mcp.NewToolResultText(string(data)), nil
		}
		if req.GetBool("stat", false) {
			text := ""
			for _, n := range result.Notes() {
				text += "# " + n + "\n"
			}
			for _, rd := range result.Resources {
				text += fmt.Sprintf("%-9s %s\n", rd.Change, rd.Resource)
			}
			return mcp.NewToolResultText(text), nil
		}
		return mcp.NewToolResultText(result.Text()), nil
	}
}

func runDiff(ctx context.Context, gw *Gateway, calls []ExecuteParams, revision string, mode appdiff.Mode) (*appdiff.Result, error) {
	var (
		app       v1alpha1.Application
		resources application.ManagedResourcesResponse
		manifests struct {
			Manifests []string `json:"manifests"`
		}
		st settings.Settings
	)
	targets := []any{&app, &resources, &manifests, &st}
	for i, c := range calls {
		if err := fetchInto(ctx, gw, c, targets[i]); err != nil {
			return nil, err
		}
	}
	return appdiff.Compute(appdiff.Inputs{App: &app, Resources: &resources, Manifests: manifests.Manifests, Settings: &st}, revision, mode)
}

// fetchInto GETs through the gateway (user token, IAP) and decodes the body.
func fetchInto(ctx context.Context, gw *Gateway, c ExecuteParams, out any) error {
	raw, err := gw.Execute(ctx, c)
	if err != nil {
		return fmt.Errorf("GET %s: %w", c.Path, err)
	}
	var res ExecuteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("GET %s: decode response: %w", c.Path, err)
	}
	if res.Status < 200 || res.Status > 299 {
		body := string(res.Body)
		if len(body) > 1000 {
			body = body[:1000] + "…"
		}
		return fmt.Errorf("GET %s returned %d: %s", c.Path, res.Status, body)
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return fmt.Errorf("GET %s: decode body: %w", c.Path, err)
	}
	return nil
}
