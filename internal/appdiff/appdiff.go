// Package appdiff computes `argocd app diff APP --revision REV` server-side,
// following the CLI's findandPrintDiff/groupObjsForDiff so results match it.
package appdiff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/argoproj/argo-cd/v3/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient/settings"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/argo"
	argodiff "github.com/argoproj/argo-cd/v3/util/argo/diff"
	"github.com/argoproj/argo-cd/v3/util/argo/normalizers"
	"github.com/argoproj/gitops-engine/pkg/sync/hook"
	"github.com/argoproj/gitops-engine/pkg/sync/ignore"
	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Mode selects what is compared.
type Mode string

const (
	ModeLive Mode = "live" // live vs. predicted after sync, like the CLI
	ModePR   Mode = "pr"   // predicted for current target vs. for the revision
)

// Inputs are the API responses the argocd CLI uses for a revision diff.
type Inputs struct {
	App       *v1alpha1.Application
	Resources *application.ManagedResourcesResponse
	Manifests []string // GET /api/v1/applications/{name}/manifests?revision=
	Settings  *settings.Settings
}

// Result is the outcome of a diff.
type Result struct {
	App              string         `json:"app"`
	Revision         string         `json:"revision"`
	Mode             Mode           `json:"mode"`
	HasDiff          bool           `json:"has_diff"`
	Counts           Counts         `json:"counts"`
	Resources        []ResourceDiff `json:"resources"`
	SyncPolicy       SyncPolicy     `json:"sync_policy"`
	PreexistingDrift []string       `json:"preexisting_drift,omitempty"`
}

// Counts tallies changed resources by kind of change.
type Counts struct {
	Modified int `json:"modified"`
	Added    int `json:"added"`
	Removed  int `json:"removed"`
}

// SyncPolicy is what a sync would do on its own.
type SyncPolicy struct {
	Automated bool `json:"automated"`
	Prune     bool `json:"prune"`
	SelfHeal  bool `json:"selfHeal"`
}

// ResourceDiff describes one changed resource.
type ResourceDiff struct {
	Resource string        `json:"resource"` // "group/Kind namespace/name", the CLI header format
	Change   string        `json:"change"`   // modified, added, removed
	Changes  []FieldChange `json:"changes,omitempty"`
	Diff     string        `json:"diff"`
}

type item struct {
	key    kube.ResourceKey
	live   *unstructured.Unstructured
	target *unstructured.Unstructured
}

// Compute diffs the application against the revision's manifests.
func Compute(in Inputs, revision string, mode Mode) (*Result, error) {
	if in.App == nil || in.Resources == nil || in.Settings == nil {
		return nil, fmt.Errorf("app, managed resources, and settings are required")
	}
	app, st := in.App, in.Settings
	destNS := app.Spec.Destination.Namespace
	appName := app.InstanceName(st.ControllerNamespace)

	liveObjs := make([]*unstructured.Unstructured, len(in.Resources.Items))
	for i, res := range in.Resources.Items {
		obj, err := res.LiveObject()
		if err != nil {
			return nil, fmt.Errorf("live object %s/%s: %w", res.Kind, res.Name, err)
		}
		liveObjs[i] = obj
	}

	var targets []*unstructured.Unstructured
	for _, m := range in.Manifests {
		obj, err := v1alpha1.UnmarshalToUnstructured(m)
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		targets = append(targets, obj)
	}
	items, err := groupForDiff(in.Resources, groupByKey(targets, liveObjs, destNS), st, appName, destNS)
	if err != nil {
		return nil, err
	}

	overrides := make(map[string]v1alpha1.ResourceOverride, len(st.ResourceOverrides))
	for k, v := range st.ResourceOverrides {
		overrides[k] = *v
	}
	// Same settings the CLI passes; IgnoreAggregatedRoles is hardcoded false there too.
	diffConfig, err := argodiff.NewDiffConfigBuilder().
		WithDiffSettings(app.Spec.IgnoreDifferences, overrides, false, normalizers.IgnoreNormalizerOpts{JQExecutionTimeout: normalizers.DefaultJQExecutionTimeout}).
		WithTracking(st.AppLabelKey, st.TrackingMethod).
		WithNoCache().
		WithLogger(logr.Discard()).
		Build()
	if err != nil {
		return nil, fmt.Errorf("build diff config: %w", err)
	}
	predict := func(live, target *unstructured.Unstructured) (*unstructured.Unstructured, bool, error) {
		if live == nil || target == nil {
			return target, live != nil || target != nil, nil
		}
		res, err := argodiff.StateDiff(live, target, diffConfig)
		if err != nil {
			return nil, false, err
		}
		predicted := &unstructured.Unstructured{}
		if err := json.Unmarshal(res.PredictedLive, predicted); err != nil {
			return nil, false, err
		}
		return predicted, res.Modified, nil
	}

	currentTargets := map[kube.ResourceKey]*unstructured.Unstructured{}
	for _, res := range in.Resources.Items {
		if res.TargetState != "" && res.TargetState != "null" {
			obj := &unstructured.Unstructured{}
			if err := json.Unmarshal([]byte(res.TargetState), obj); err != nil {
				return nil, fmt.Errorf("target state %s/%s: %w", res.Kind, res.Name, err)
			}
			currentTargets[kube.NewResourceKey(res.Group, res.Kind, res.Namespace, res.Name)] = obj
		}
	}

	result := &Result{App: app.Name, Revision: revision, Mode: mode, SyncPolicy: syncPolicy(app.Spec.SyncPolicy)}
	for _, it := range items {
		if it.target != nil && hook.IsHook(it.target) || it.live != nil && hook.IsHook(it.live) {
			continue
		}
		after, modified, err := predict(it.live, it.target)
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", label(it.key), err)
		}
		before := it.live
		if mode == ModePR {
			current := currentTargets[it.key]
			if current == nil && it.target == nil {
				continue // live-only resource: not caused by this revision
			}
			var drifted bool
			if before, drifted, err = predict(it.live, current); err != nil {
				return nil, fmt.Errorf("diff %s against current target: %w", label(it.key), err)
			}
			if drifted && it.live != nil && current != nil {
				result.PreexistingDrift = append(result.PreexistingDrift, label(it.key))
			}
			modified = !jsonEqual(content(before), content(after))
		}
		if before == nil && after == nil || !modified && before != nil && after != nil {
			continue
		}
		rd := ResourceDiff{Resource: label(it.key)}
		switch {
		case before == nil:
			rd.Change, result.Counts.Added = "added", result.Counts.Added+1
		case after == nil:
			rd.Change, result.Counts.Removed = "removed", result.Counts.Removed+1
		default:
			rd.Change, result.Counts.Modified = "modified", result.Counts.Modified+1
			rd.Changes = fieldChanges(before.Object, after.Object, "")
		}
		if rd.Diff, err = unifiedDiff(rd.Resource, it.key.Name, before, after); err != nil {
			return nil, err
		}
		result.Resources = append(result.Resources, rd)
	}
	sort.Slice(result.Resources, func(i, j int) bool { return result.Resources[i].Resource < result.Resources[j].Resource })
	sort.Strings(result.PreexistingDrift)
	result.HasDiff = len(result.Resources) > 0
	return result, nil
}

// groupByKey mirrors the CLI's groupObjsByKey: namespaces are inferred from
// live objects, and a kind with no live instance counts as cluster-scoped.
func groupByKey(targets, liveObjs []*unstructured.Unstructured, appNamespace string) map[kube.ResourceKey]*unstructured.Unstructured {
	namespacedByGk := make(map[schema.GroupKind]bool)
	for _, live := range liveObjs {
		if live != nil {
			key := kube.GetResourceKey(live)
			namespacedByGk[schema.GroupKind{Group: key.Group, Kind: key.Kind}] = key.Namespace != ""
		}
	}
	// Mirrors controller.DeduplicateTargetObjects with the CLI's resourceInfoProvider.
	byKey := make(map[kube.ResourceKey]*unstructured.Unstructured)
	for i, obj := range targets {
		if obj == nil {
			continue
		}
		if !namespacedByGk[obj.GroupVersionKind().GroupKind()] {
			obj.SetNamespace("")
		} else if obj.GetNamespace() == "" {
			obj.SetNamespace(appNamespace)
		}
		key := kube.GetResourceKey(obj)
		if key.Name == "" && obj.GetGenerateName() != "" {
			key.Name = fmt.Sprintf("%s%d", obj.GetGenerateName(), i)
		}
		if !hook.IsHook(obj) && !ignore.Ignore(obj) {
			byKey[key] = obj // last duplicate wins, as in the controller
		}
	}
	return byKey
}

// groupForDiff mirrors the CLI's groupObjsForDiff, including skipping Secrets
// (Argo CD has no access to their data).
func groupForDiff(resources *application.ManagedResourcesResponse, objs map[kube.ResourceKey]*unstructured.Unstructured, st *settings.Settings, appName, namespace string) ([]item, error) {
	tracking := argo.NewResourceTracking()
	var items []item
	for _, res := range resources.Items {
		var live *unstructured.Unstructured
		if err := json.Unmarshal([]byte(res.NormalizedLiveState), &live); err != nil {
			return nil, fmt.Errorf("normalized live state %s/%s: %w", res.Kind, res.Name, err)
		}
		key := kube.ResourceKey{Name: res.Name, Namespace: res.Namespace, Group: res.Group, Kind: res.Kind}
		if key.Kind == kube.SecretKind && key.Group == "" {
			delete(objs, key)
			continue
		}
		if local, ok := objs[key]; ok || live != nil {
			if local != nil && !kube.IsCRD(local) {
				if err := tracking.SetAppInstance(local, st.AppLabelKey, appName, namespace, v1alpha1.TrackingMethod(st.GetTrackingMethod()), st.GetInstallationID()); err != nil {
					return nil, err
				}
			}
			items = append(items, item{key, live, local})
			delete(objs, key)
		}
	}
	for key, local := range objs {
		if key.Kind == kube.SecretKind && key.Group == "" {
			continue
		}
		items = append(items, item{key, nil, local})
	}
	return items, nil
}

func syncPolicy(p *v1alpha1.SyncPolicy) SyncPolicy {
	if p == nil || p.Automated == nil || (p.Automated.Enabled != nil && !*p.Automated.Enabled) {
		return SyncPolicy{}
	}
	return SyncPolicy{Automated: true, Prune: p.Automated.Prune, SelfHeal: p.Automated.SelfHeal}
}

func label(k kube.ResourceKey) string {
	return fmt.Sprintf("%s/%s %s/%s", k.Group, k.Kind, k.Namespace, k.Name)
}

func content(obj *unstructured.Unstructured) map[string]any {
	if obj == nil {
		return nil
	}
	return obj.Object
}

// Notes returns summary lines: counts, sync policy, prune, drift.
func (r *Result) Notes() []string {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	notes := []string{fmt.Sprintf("%d resource(s) differ: %d modified, %d added, %d removed",
		len(r.Resources), r.Counts.Modified, r.Counts.Added, r.Counts.Removed),
		fmt.Sprintf("sync policy: automated=%s, prune=%s, selfHeal=%s",
			onOff(r.SyncPolicy.Automated), onOff(r.SyncPolicy.Prune), onOff(r.SyncPolicy.SelfHeal))}
	if !r.SyncPolicy.Automated {
		notes = append(notes, "note: automated sync is off; nothing applies until a manual sync")
	}
	if r.Counts.Removed > 0 && !r.SyncPolicy.Prune {
		notes = append(notes, fmt.Sprintf("note: prune is off; %d removed resource(s) would be orphaned", r.Counts.Removed))
	}
	if len(r.PreexistingDrift) > 0 {
		notes = append(notes, fmt.Sprintf("note: already drifted (drift hidden in pr mode; the revision's changes still show): %s",
			strings.Join(r.PreexistingDrift, ", ")))
	}
	return notes
}
