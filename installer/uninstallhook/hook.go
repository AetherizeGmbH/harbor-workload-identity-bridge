// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// installContainer is the DaemonSet's init container that runs the
// installer pass, and syncContainer its long-running one (the chart's
// plugin-daemonset.yaml).
const (
	installContainer = "install"
	syncContainer    = "sync"
	actionEnv        = "INSTALL_ACTION"
	actionRemove     = "remove"
)

// removalReport is the installer's report of a removal pass, which it
// writes as the termination message of the install container. It
// duplicates the installer's type (installer/remove.go) on purpose, as the
// plugin duplicates the bridge's wire types (ADR-0015); both tests read
// installer/testdata/removal-report.golden.json.
type removalReport struct {
	Action           string   `json:"action"`
	Provider         string   `json:"provider"`
	Result           string   `json:"result"`
	KubeletRestarted bool     `json:"kubeletRestarted"`
	Kept             []string `json:"kept,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// removalPatch is the strategic merge patch that switches the DaemonSet to
// the removal action: INSTALL_ACTION=remove in both containers, which the
// chart renders as "install", and the rollout's budget (budget).
func removalPatch(maxUnavailable int) ([]byte, error) {
	env := []map[string]string{{"name": actionEnv, "value": actionRemove}}
	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"updateStrategy": map[string]any{
				"type":          "RollingUpdate",
				"rollingUpdate": map[string]any{"maxUnavailable": maxUnavailable},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"initContainers": []map[string]any{{"name": installContainer, "env": env}},
					"containers":     []map[string]any{{"name": syncContainer, "env": env}},
				},
			},
		},
	})
}

// api is what run needs of the API server; apiClient implements it.
type api interface {
	getDaemonSet(ctx context.Context, namespace, name string) (*daemonSet, error)
	patchDaemonSet(ctx context.Context, namespace, name string, patch []byte) error
	listPods(ctx context.Context, namespace string, selector map[string]string) ([]pod, error)
}

// run switches the DaemonSet to the removal action and waits until every
// node reported its removal (ADR-0039, decision 5), writing its progress
// and the per-node results to out.
func run(ctx context.Context, c api, cfg *config, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	var ds *daemonSet
	err := retry(ctx, cfg, out, func() (err error) {
		ds, err = c.getDaemonSet(ctx, cfg.Namespace, cfg.DaemonSet)
		return err
	})
	switch {
	case errors.Is(err, errNotFound):
		_, _ = fmt.Fprintf(out, "DaemonSet %s/%s does not exist: nothing to remove on the nodes. If it was deleted by hand, clean the nodes by hand (docs/operations.md, Uninstalling).\n", cfg.Namespace, cfg.DaemonSet)
		return nil
	case err != nil:
		return err
	}
	// Tools that render the chart with helm template (kubectl apply,
	// Kustomize, kapp, Argo CD) apply this hook like any other object, or at
	// a time of their choosing: removing the plugin then would take it off
	// every node of a running install. Only a DaemonSet that a Helm release
	// manages, and the release this hook belongs to, is switched.
	if name, ns := ds.Metadata.Annotations[helmReleaseNameAnnotation], ds.Metadata.Annotations[helmReleaseNamespaceAnnotation]; name != cfg.ReleaseName || ns != cfg.ReleaseNamespace {
		_, _ = fmt.Fprintf(out, "the DaemonSet %s/%s is not managed by Helm release %s/%s (its %s and %s annotations say %q and %q): rendered with helm template? Nothing is removed on the nodes; set plugin.uninstall.enabled=false or render with --no-hooks (docs/operations.md, Uninstalling)\n",
			cfg.Namespace, cfg.DaemonSet, cfg.ReleaseNamespace, cfg.ReleaseName, helmReleaseNamespaceAnnotation, helmReleaseNameAnnotation, ns, name)
		return nil
	}
	n, err := budget(cfg.MaxUnavailable, ds.Status.DesiredNumberScheduled, ds.Status.NumberUnavailable)
	if err != nil {
		return err
	}
	patch, err := removalPatch(n)
	if err != nil {
		return err
	}
	if err := retry(ctx, cfg, out, func() error { return c.patchDaemonSet(ctx, cfg.Namespace, cfg.DaemonSet, patch) }); err != nil {
		return fmt.Errorf("switch DaemonSet %s/%s to the removal action: %w", cfg.Namespace, cfg.DaemonSet, err)
	}
	_, _ = fmt.Fprintf(out, "switched DaemonSet %s/%s to INSTALL_ACTION=remove (at most %d nodes at once); waiting until every node removed provider %q\n", cfg.Namespace, cfg.DaemonSet, n, cfg.Provider)

	last := ""
	for {
		ds, err := c.getDaemonSet(ctx, cfg.Namespace, cfg.DaemonSet)
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("DaemonSet %s/%s was deleted while the nodes removed provider %q; the nodes without a report below may still have it: %s", cfg.Namespace, cfg.DaemonSet, cfg.Provider, last)
		}
		var pods []pod
		if err == nil {
			pods, err = c.listPods(ctx, cfg.Namespace, ds.Spec.Selector.MatchLabels)
		}
		if err != nil {
			if ctx.Err() != nil {
				return timeoutError(cfg, last)
			}
			// The API server may be briefly unreachable; the deadline bounds
			// the retries.
			_, _ = fmt.Fprintf(out, "%v; trying again\n", err)
		} else {
			p := evaluate(ds, pods, cfg.Provider)
			if p.failed != nil {
				printNodes(out, p.nodes)
				return fmt.Errorf("node %s: %s. The other nodes keep removing, because the DaemonSet's rollout goes on; fix that node and run helm uninstall again, which continues (docs/operations.md, Uninstalling)", p.failed.node, p.failed.detail)
			}
			if p.done {
				printNodes(out, p.nodes)
				_, _ = fmt.Fprintf(out, "every node (%d) removed provider %q\n", len(p.nodes), cfg.Provider)
				return nil
			}
			if s := p.summary(); s != last {
				_, _ = fmt.Fprintln(out, s)
				last = s
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				printNodes(out, evaluateOrNil(ctx, c, cfg))
				return timeoutError(cfg, last)
			}
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// The annotations Helm puts on every object of a release (helm.sh/helm
// pkg/action/validate.go, setMetadataVisitor); helm template does not.
const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// retry calls f until it succeeds, answers errNotFound, or the deadline of
// ctx passes: the API server may be briefly unreachable, as in the polling
// loop of run.
func retry(ctx context.Context, cfg *config, out io.Writer, f func() error) error {
	for {
		err := f()
		if err == nil || errors.Is(err, errNotFound) {
			return err
		}
		_, _ = fmt.Fprintf(out, "%v; trying again\n", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(cfg.PollInterval):
		}
	}
}

func timeoutError(cfg *config, last string) error {
	return fmt.Errorf("not every node removed provider %q within %s (%s). The DaemonSet stays in the removal action and its rollout goes on; run helm uninstall again to continue, or helm uninstall --no-hooks to give up, and clean the nodes listed above by hand (docs/operations.md, Uninstalling)", cfg.Provider, cfg.Timeout, last)
}

// evaluateOrNil is the node list at the deadline, read with a short
// deadline of its own; nil when the API server does not answer.
func evaluateOrNil(ctx context.Context, c api, cfg *config) []nodeState {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ds, err := c.getDaemonSet(ctx, cfg.Namespace, cfg.DaemonSet)
	if err != nil {
		return nil
	}
	pods, err := c.listPods(ctx, cfg.Namespace, ds.Spec.Selector.MatchLabels)
	if err != nil {
		return nil
	}
	return evaluate(ds, pods, cfg.Provider).nodes
}

// nodeState is what the hook knows about one node's removal.
type nodeState struct {
	node   string
	state  string // removed, nothing, kept, failed, pending
	detail string
}

// progress is the state of the removal rollout.
type progress struct {
	nodes []nodeState
	// failed is the first node that reported a failed removal.
	failed *nodeState
	// done is set when the rollout is done as kubectl rollout status
	// defines it and every node reported its removal.
	done bool
}

func (p progress) summary() string {
	counts := map[string]int{}
	for _, n := range p.nodes {
		counts[n.state]++
	}
	return fmt.Sprintf("%d nodes: %d removed, %d had nothing to remove, %d kept files, %d pending",
		len(p.nodes), counts["removed"], counts["nothing"], counts["kept"], counts["pending"])
}

// evaluate reads the removal's progress from the DaemonSet and its pods.
// A pod reports through the termination message of its install container
// (removalReport); a pod that still has the install action, or is being
// deleted, has not reported yet.
func evaluate(ds *daemonSet, pods []pod, provider string) progress {
	var p progress
	reported := 0
	for i := range pods {
		n := nodeOf(&pods[i], provider)
		if n.state != "pending" {
			reported++
		}
		p.nodes = append(p.nodes, n)
	}
	sort.Slice(p.nodes, func(i, j int) bool { return p.nodes[i].node < p.nodes[j].node })
	for i := range p.nodes {
		if p.nodes[i].state == "failed" {
			p.failed = &p.nodes[i]
			return p
		}
	}
	st := ds.Status
	rolledOut := ds.Metadata.Generation <= st.ObservedGeneration &&
		st.UpdatedNumberScheduled >= st.DesiredNumberScheduled && st.NumberAvailable >= st.DesiredNumberScheduled
	p.done = rolledOut && reported >= st.DesiredNumberScheduled && reported == len(p.nodes)
	return p
}

// nodeOf is the removal state of the node pod runs on.
func nodeOf(pd *pod, provider string) nodeState {
	n := nodeState{node: pd.Spec.NodeName, state: "pending"}
	if n.node == "" {
		n.node = "(pod " + pd.Metadata.Name + ", not scheduled)"
	}
	switch {
	case pd.Metadata.DeletionTimestamp != nil:
		n.detail = "its pod is being deleted (a NotReady or unreachable node keeps it so)"
		return n
	case !removalAction(pd):
		n.detail = "its pod still runs the install action; the rollout has not replaced it yet"
		return n
	}
	var cs *containerStatus
	for i := range pd.Status.InitContainerStatuses {
		if pd.Status.InitContainerStatuses[i].Name == installContainer {
			cs = &pd.Status.InitContainerStatuses[i]
		}
	}
	if cs == nil {
		n.detail = "its pod has not started (phase " + pd.Status.Phase + ")"
		return n
	}
	term := cs.State.Terminated
	if term == nil && cs.State.Waiting != nil && cs.LastState.Terminated != nil {
		// CrashLoopBackOff: the last run failed.
		term = cs.LastState.Terminated
	}
	if term == nil {
		n.detail = "removing"
		return n
	}
	var rep removalReport
	parsed := json.Unmarshal([]byte(strings.TrimSpace(term.Message)), &rep) == nil && rep.Action == actionRemove && rep.Provider == provider
	switch {
	case !parsed && term.ExitCode == 0:
		n.state, n.detail = "failed", "its installer finished without a removal report: the plugin image (plugin.image) predates ADR-0039 and ran an install pass instead; set plugin.image to a version with ADR-0039"
	case !parsed:
		n.state, n.detail = "failed", fmt.Sprintf("its installer failed (exit code %d, %s): %s", term.ExitCode, term.Reason, strings.TrimSpace(term.Message))
	case term.ExitCode != 0 || rep.Result == "failed":
		n.state, n.detail = "failed", "removal failed: "+rep.Error
	default:
		n.state = rep.Result
		var parts []string
		if rep.KubeletRestarted {
			parts = append(parts, "kubelet restarted")
		}
		if len(rep.Kept) > 0 {
			parts = append(parts, "kept "+strings.Join(rep.Kept, "; "))
		}
		n.detail = strings.Join(parts, ", ")
	}
	return n
}

// removalAction reports whether pod's install container runs the removal
// action.
func removalAction(pd *pod) bool {
	for _, c := range pd.Spec.InitContainers {
		if c.Name != installContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == actionEnv {
				return e.Value == actionRemove
			}
		}
	}
	return false
}

func printNodes(out io.Writer, nodes []nodeState) {
	for _, n := range nodes {
		line := "node " + n.node + ": " + n.state
		if n.detail != "" {
			line += " (" + n.detail + ")"
		}
		_, _ = fmt.Fprintln(out, line)
	}
}
