package appdiff

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/v3/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/settings"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	appName = "demo"
	ns      = "demo-ns"
)

func testApp(automated *v1alpha1.SyncPolicyAutomated) *v1alpha1.Application {
	app := &v1alpha1.Application{}
	app.Name = appName
	app.Spec.Destination.Namespace = ns
	if automated != nil {
		app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: automated}
	}
	return app
}

func testSettings() *settings.Settings {
	return &settings.Settings{AppLabelKey: "argocd.argoproj.io/instance", TrackingMethod: "annotation", ControllerNamespace: "argocd"}
}

func configMap(name string, data map[string]any, extraMeta map[string]any) map[string]any {
	meta := map[string]any{
		"name":      name,
		"namespace": ns,
		"annotations": map[string]any{
			"argocd.argoproj.io/tracking-id": appName + ":/ConfigMap:" + ns + "/" + name,
		},
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": data}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// managed builds a managed-resources entry whose live and target states are given.
func managed(t *testing.T, kind, name string, live, target map[string]any) *v1alpha1.ResourceDiff {
	t.Helper()
	rd := &v1alpha1.ResourceDiff{Kind: kind, Namespace: ns, Name: name}
	if live != nil {
		rd.LiveState = toJSON(t, live)
		rd.NormalizedLiveState = rd.LiveState
		rd.PredictedLiveState = rd.LiveState
	} else {
		rd.LiveState, rd.NormalizedLiveState = "null", "null"
	}
	if target != nil {
		rd.TargetState = toJSON(t, target)
	}
	return rd
}

// rendered drops metadata.namespace, as repo-server output often does.
func rendered(t *testing.T, obj map[string]any) string {
	t.Helper()
	cp := map[string]any{}
	if err := json.Unmarshal([]byte(toJSON(t, obj)), &cp); err != nil {
		t.Fatal(err)
	}
	delete(cp["metadata"].(map[string]any), "namespace")
	return toJSON(t, cp)
}

func TestUnchangedTargetIsNoDiff(t *testing.T) {
	cm := configMap("cfg", map[string]any{"a": "1"}, nil)
	in := Inputs{
		App:       testApp(nil),
		Resources: &application.ManagedResourcesResponse{Items: []*v1alpha1.ResourceDiff{managed(t, "ConfigMap", "cfg", cm, cm)}},
		Manifests: []string{rendered(t, cm)},
		Settings:  testSettings(),
	}
	for _, mode := range []Mode{ModeLive, ModePR} {
		r, err := Compute(in, "rev", mode)
		if err != nil {
			t.Fatal(err)
		}
		if r.HasDiff {
			t.Errorf("%s: expected no diff, got %s", mode, r.Text())
		}
	}
}

func TestModifiedAddedRemovedAndSkipped(t *testing.T) {
	live := configMap("cfg", map[string]any{"a": "1"}, nil)
	next := configMap("cfg", map[string]any{"a": "2"}, nil)
	gone := configMap("old", map[string]any{"x": "y"}, nil)
	added := configMap("new", map[string]any{"b": "1"}, nil)
	hookCM := configMap("hook", map[string]any{}, map[string]any{
		"annotations": map[string]any{"helm.sh/hook": "pre-install"},
	})
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"k":"dg=="}}`

	in := Inputs{
		App: testApp(&v1alpha1.SyncPolicyAutomated{}),
		Resources: &application.ManagedResourcesResponse{Items: []*v1alpha1.ResourceDiff{
			managed(t, "ConfigMap", "cfg", live, live),
			managed(t, "ConfigMap", "old", gone, gone),
		}},
		Manifests: []string{rendered(t, next), rendered(t, added), rendered(t, hookCM), secret},
		Settings:  testSettings(),
	}
	r, err := Compute(in, "rev", ModeLive)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counts != (Counts{Modified: 1, Added: 1, Removed: 1}) {
		t.Fatalf("counts = %+v\n%s", r.Counts, r.Text())
	}
	byName := map[string]ResourceDiff{}
	for _, rd := range r.Resources {
		byName[rd.Resource] = rd
	}
	mod := byName["/ConfigMap demo-ns/cfg"]
	if mod.Change != "modified" || len(mod.Changes) != 1 || mod.Changes[0].Path != "/data/a" {
		t.Errorf("modified cfg: %+v", mod)
	}
	if !strings.Contains(mod.Diff, "-  a: \"1\"") || !strings.Contains(mod.Diff, "+  a: \"2\"") {
		t.Errorf("diff body:\n%s", mod.Diff)
	}
	if byName["/ConfigMap demo-ns/old"].Change != "removed" {
		t.Errorf("expected old removed: %v", byName)
	}
	if byName["/ConfigMap demo-ns/new"].Change != "added" {
		t.Errorf("expected new added under the destination namespace: %v", byName)
	}
	for res := range byName {
		if strings.Contains(res, "Secret") || strings.Contains(res, "hook") {
			t.Errorf("secrets and hooks must be skipped, got %s", res)
		}
	}
	if !strings.Contains(r.Text(), "prune is off") {
		t.Errorf("expected prune note:\n%s", r.Text())
	}
}

func TestNewKindIsTreatedAsClusterScoped(t *testing.T) {
	// The CLI infers namespacing from live objects; a kind with no live
	// instance counts as cluster-scoped, so its namespace is cleared.
	pdb := `{"apiVersion":"policy/v1","kind":"PodDisruptionBudget","metadata":{"name":"web"},"spec":{"maxUnavailable":1}}`
	in := Inputs{
		App:       testApp(nil),
		Resources: &application.ManagedResourcesResponse{},
		Manifests: []string{pdb},
		Settings:  testSettings(),
	}
	r, err := Compute(in, "rev", ModeLive)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Resources) != 1 || r.Resources[0].Resource != "policy/PodDisruptionBudget /web" {
		t.Fatalf("resources: %+v", r.Resources)
	}
}

func TestPRModeExcludesDrift(t *testing.T) {
	target := configMap("cfg", map[string]any{"a": "1"}, nil)
	drifted := configMap("cfg", map[string]any{"a": "manual"}, nil)
	in := Inputs{
		App:       testApp(nil),
		Resources: &application.ManagedResourcesResponse{Items: []*v1alpha1.ResourceDiff{managed(t, "ConfigMap", "cfg", drifted, target)}},
		Manifests: []string{rendered(t, target)},
		Settings:  testSettings(),
	}
	live, err := Compute(in, "rev", ModeLive)
	if err != nil {
		t.Fatal(err)
	}
	if !live.HasDiff {
		t.Error("live mode should show the drift")
	}
	pr, err := Compute(in, "rev", ModePR)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HasDiff || len(pr.PreexistingDrift) != 1 {
		t.Errorf("pr mode should exclude and report drift: %+v", pr)
	}
}

func TestPRModeShowsRevisionChangesOnDriftedResource(t *testing.T) {
	// Live drifted on "a"; the revision changes "b". pr mode shows only "b".
	target := configMap("cfg", map[string]any{"a": "1", "b": "1"}, nil)
	drifted := configMap("cfg", map[string]any{"a": "manual", "b": "1"}, nil)
	next := configMap("cfg", map[string]any{"a": "1", "b": "2"}, nil)
	in := Inputs{
		App:       testApp(nil),
		Resources: &application.ManagedResourcesResponse{Items: []*v1alpha1.ResourceDiff{managed(t, "ConfigMap", "cfg", drifted, target)}},
		Manifests: []string{rendered(t, next)},
		Settings:  testSettings(),
	}
	r, err := Compute(in, "rev", ModePR)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.PreexistingDrift) != 1 || len(r.Resources) != 1 {
		t.Fatalf("want the drifted resource listed and diffed: %+v", r)
	}
	changes := r.Resources[0].Changes
	if len(changes) != 1 || changes[0].Path != "/data/b" {
		t.Errorf("pr mode should show only the revision's change, got %+v", changes)
	}
}

func TestFieldChangesPairListItemsByDetectedKey(t *testing.T) {
	// Items keyed by a field no fixed key list would know about, reordered, one edited.
	before := map[string]any{"backends": []any{
		map[string]any{"id": "a", "url": "x"}, map[string]any{"id": "b", "url": "y"},
	}}
	after := map[string]any{"backends": []any{
		map[string]any{"id": "b", "url": "y2"}, map[string]any{"id": "a", "url": "x"},
	}}
	got := fieldChanges(before, after, "")
	if len(got) != 1 || got[0].Path != "/backends/0/url" || got[0].Before != "y" || got[0].After != "y2" {
		t.Errorf("changes = %+v", got)
	}
}
