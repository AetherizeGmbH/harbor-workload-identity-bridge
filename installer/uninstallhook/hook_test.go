// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a DaemonSet with pods. patched is set by the hook's patch; a
// pod becomes a removal pod (with report) once the rollout reaches it.
type fakeAPI struct {
	mu      sync.Mutex
	ds      *daemonSet
	pods    []pod
	patches [][]byte
	// onPatch runs after the patch: it rolls the pods out.
	onPatch func(f *fakeAPI)
	listErr error
}

func (f *fakeAPI) getDaemonSet(_ context.Context, _, _ string) (*daemonSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ds == nil {
		return nil, errNotFound
	}
	ds := *f.ds
	return &ds, nil
}

func (f *fakeAPI) patchDaemonSet(_ context.Context, _, _ string, patch []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patch)
	f.ds.Metadata.Generation++
	if f.onPatch != nil {
		f.onPatch(f)
	}
	return nil
}

func (f *fakeAPI) listPods(context.Context, string, map[string]string) ([]pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pod(nil), f.pods...), f.listErr
}

func newDaemonSet(desired int) *daemonSet {
	ds := &daemonSet{}
	ds.Metadata.Generation = 1
	ds.Spec.Selector.MatchLabels = map[string]string{"app.kubernetes.io/component": "plugin"}
	ds.Status.ObservedGeneration = 1
	ds.Status.DesiredNumberScheduled = desired
	ds.Status.UpdatedNumberScheduled = desired
	ds.Status.NumberAvailable = desired
	return ds
}

// podOn is a pod of the DaemonSet on node, with the action and, when
// message is not nil, a terminated install container.
func podOn(node, action string, exitCode int, message *string) pod {
	var p pod
	p.Metadata.Name = "plugin-" + node
	p.Spec.NodeName = node
	c := container{Name: installContainer}
	c.Env = append(c.Env, struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{Name: actionEnv, Value: action})
	p.Spec.InitContainers = []container{c}
	p.Status.Phase = "Running"
	cs := containerStatus{Name: installContainer}
	if message != nil {
		cs.State.Terminated = &struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
			Message  string `json:"message"`
		}{ExitCode: exitCode, Reason: "Completed", Message: *message}
	} else {
		cs.State.Running = &struct{}{}
	}
	p.Status.InitContainerStatuses = []containerStatus{cs}
	return p
}

func report(t *testing.T, rep removalReport) *string {
	t.Helper()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw) + "\n"
	return &s
}

func testConfig() *config {
	return &config{Namespace: "harbor-bridge-system", DaemonSet: "harbor-bridge-plugin", Provider: "harbor-bridge-plugin",
		MaxUnavailable: "25%", Timeout: 2 * time.Second, PollInterval: time.Millisecond}
}

// rollOut makes every pod a removal pod with rep as its report, and the
// DaemonSet rolled out.
func rollOut(rep *string) func(f *fakeAPI) {
	return func(f *fakeAPI) {
		for i, p := range f.pods {
			f.pods[i] = podOn(p.Spec.NodeName, actionRemove, 0, rep)
		}
		f.ds.Status.ObservedGeneration = f.ds.Metadata.Generation
	}
}

// TestRun_Done: the hook patches the DaemonSet once and succeeds when every
// node reported, printing one line per node.
func TestRun_Done(t *testing.T) {
	f := &fakeAPI{ds: newDaemonSet(3)}
	f.ds.Status.NumberUnavailable = 1
	for _, n := range []string{"c", "a", "b"} {
		f.pods = append(f.pods, podOn(n, "install", 0, nil))
	}
	f.onPatch = rollOut(report(t, removalReport{Action: actionRemove, Provider: "harbor-bridge-plugin", Result: "removed", KubeletRestarted: true}))
	var out bytes.Buffer
	if err := run(context.Background(), f, testConfig(), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if len(f.patches) != 1 {
		t.Fatalf("patches = %d", len(f.patches))
	}
	// 25% of 3, rounded up, plus the one unavailable pod.
	want, err := removalPatch(2)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.patches[0]) != string(want) {
		t.Fatalf("patch = %s, want %s", f.patches[0], want)
	}
	if !strings.Contains(out.String(), "node a: removed (kubelet restarted)\nnode b: removed (kubelet restarted)\nnode c: removed") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// TestRemovalPatch pins the patch: INSTALL_ACTION in both containers, by
// container name, and the budget as a number.
func TestRemovalPatch(t *testing.T) {
	got, err := removalPatch(3)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"INSTALL_ACTION","value":"remove"}],"name":"sync"}],` +
		`"initContainers":[{"env":[{"name":"INSTALL_ACTION","value":"remove"}],"name":"install"}]}},` +
		`"updateStrategy":{"rollingUpdate":{"maxUnavailable":3},"type":"RollingUpdate"}}}`
	if string(got) != want {
		t.Fatalf("patch =\n%s\nwant\n%s", got, want)
	}
}

func TestRun_NoDaemonSet(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &fakeAPI{}, testConfig(), &out); err != nil || !strings.Contains(out.String(), "does not exist") {
		t.Fatalf("got %v\n%s", err, out.String())
	}
}

// TestRun_FailsFast: one node reports a failed removal; the hook fails at
// once, naming the node and its error, while other nodes are still
// removing.
func TestRun_FailsFast(t *testing.T) {
	f := &fakeAPI{ds: newDaemonSet(2)}
	f.pods = []pod{podOn("a", "install", 0, nil), podOn("b", "install", 0, nil)}
	failed := report(t, removalReport{Action: actionRemove, Provider: "harbor-bridge-plugin", Result: "failed", Error: "kubelet would not start"})
	f.onPatch = func(f *fakeAPI) {
		f.pods = []pod{podOn("a", actionRemove, 1, failed), podOn("b", actionRemove, 0, nil)}
	}
	cfg := testConfig()
	cfg.Timeout = time.Minute
	start := time.Now()
	var out bytes.Buffer
	err := run(context.Background(), f, cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "node a: removal failed: kubelet would not start") || !strings.Contains(err.Error(), "keep removing") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the hook waited for its deadline")
	}
}

// TestRun_Timeout: a node that never reports fails the hook at its
// deadline, and the output lists it.
func TestRun_Timeout(t *testing.T) {
	f := &fakeAPI{ds: newDaemonSet(2)}
	f.pods = []pod{podOn("a", "install", 0, nil), podOn("b", "install", 0, nil)}
	ok := report(t, removalReport{Action: actionRemove, Provider: "harbor-bridge-plugin", Result: "nothing"})
	f.onPatch = func(f *fakeAPI) {
		stuck := podOn("b", "install", 0, nil)
		ts := "2026-11-08T12:00:00Z"
		stuck.Metadata.DeletionTimestamp = &ts
		f.pods = []pod{podOn("a", actionRemove, 0, ok), stuck}
	}
	cfg := testConfig()
	cfg.Timeout = 200 * time.Millisecond
	var out bytes.Buffer
	err := run(context.Background(), f, cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "within 200ms") || !strings.Contains(err.Error(), "run helm uninstall again") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "node b: pending (its pod is being deleted") || !strings.Contains(out.String(), "node a: nothing") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// TestEvaluate covers the states a pod can report.
func TestEvaluate(t *testing.T) {
	const provider = "harbor-bridge-plugin"
	golden, err := os.ReadFile(filepath.Join("..", "testdata", "removal-report.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fromInstaller removalReport
	if err := json.Unmarshal(golden, &fromInstaller); err != nil {
		t.Fatal(err)
	}
	// The installer's golden report decodes into the hook's copy of the
	// type with every field.
	if raw, _ := json.Marshal(fromInstaller); strings.TrimSpace(string(golden)) != string(raw) {
		t.Fatalf("the hook's removalReport differs from the installer's:\n%s\n%s", golden, raw)
	}
	g := string(golden)
	empty := ""
	crash := podOn("a", actionRemove, 0, nil)
	crash.Status.InitContainerStatuses[0].State = containerState{Waiting: &struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}{Reason: "CrashLoopBackOff"}}
	crashMsg := "harbor-bridge-installer: remove: lock: timed out"
	crash.Status.InitContainerStatuses[0].LastState.Terminated = &struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
	}{ExitCode: 1, Reason: "Error", Message: crashMsg}

	for name, tc := range map[string]struct {
		pod    pod
		state  string
		detail string
	}{
		"install action":       {podOn("a", "install", 0, &empty), "pending", "still runs the install action"},
		"removing":             {podOn("a", actionRemove, 0, nil), "pending", "removing"},
		"kept":                 {podOn("a", actionRemove, 0, report(t, removalReport{Action: actionRemove, Provider: provider, Result: "kept", Kept: []string{"x: y"}})), "kept", "kept x: y"},
		"old image":            {podOn("a", actionRemove, 0, &empty), "failed", "plugin.image"},
		"other provider":       {podOn("a", actionRemove, 0, &g), "failed", "plugin.image"},
		"crash loop":           {crash, "failed", crashMsg},
		"failed with a report": {podOn("a", actionRemove, 1, report(t, removalReport{Action: actionRemove, Provider: provider, Result: "failed", Error: "boom"})), "failed", "removal failed: boom"},
	} {
		n := nodeOf(&tc.pod, provider)
		if n.state != tc.state || !strings.Contains(n.detail, tc.detail) {
			t.Errorf("%s: %+v, want %s containing %q", name, n, tc.state, tc.detail)
		}
	}

	// Done needs the rollout observed and complete, and a report from every
	// pod, at least as many as the DaemonSet wants.
	ok := report(t, removalReport{Action: actionRemove, Provider: provider, Result: "removed"})
	ds := newDaemonSet(2)
	pods := []pod{podOn("a", actionRemove, 0, ok), podOn("b", actionRemove, 0, ok)}
	if p := evaluate(ds, pods, provider); !p.done {
		t.Fatalf("not done: %+v", p)
	}
	for name, change := range map[string]func(*daemonSet) *daemonSet{
		"patch not observed": func(d *daemonSet) *daemonSet { d.Metadata.Generation = 2; return d },
		"not all updated":    func(d *daemonSet) *daemonSet { d.Status.UpdatedNumberScheduled = 1; return d },
		"not all available":  func(d *daemonSet) *daemonSet { d.Status.NumberAvailable = 1; return d },
		"a node more":        func(d *daemonSet) *daemonSet { d.Status.DesiredNumberScheduled = 3; return d },
	} {
		if p := evaluate(change(newDaemonSet(2)), pods, provider); p.done {
			t.Errorf("%s: done", name)
		}
	}
	if p := evaluate(newDaemonSet(0), nil, provider); !p.done {
		t.Error("a DaemonSet without nodes is not done")
	}
}

func TestBudget(t *testing.T) {
	for _, tc := range []struct {
		maxUnavailable       string
		desired, unavailable int
		want                 int
	}{
		{"25%", 3, 0, 1}, {"25%", 4, 0, 1}, {"25%", 5, 0, 2}, {"25%", 100, 0, 25}, {"25%", 3, 1, 2},
		{"25%", 3, 5, 3}, {"100%", 7, 0, 7}, {"1%", 1, 0, 1}, {"2", 10, 0, 2}, {"2", 10, 3, 5}, {"25%", 0, 0, 1},
	} {
		if got, err := budget(tc.maxUnavailable, tc.desired, tc.unavailable); err != nil || got != tc.want {
			t.Errorf("budget(%q, %d, %d) = %d, %v; want %d", tc.maxUnavailable, tc.desired, tc.unavailable, got, err, tc.want)
		}
	}
	for _, bad := range []string{"0", "0%", "101%", "-1", "x", "25.5%", ""} {
		if _, err := budget(bad, 3, 0); err == nil {
			t.Errorf("budget(%q) accepted", bad)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	base := map[string]string{"NAMESPACE": "harbor-bridge-system", "DAEMONSET": "harbor-bridge-plugin", "PROVIDER_NAME": "harbor-bridge-plugin"}
	c, err := loadConfig(env(base))
	if err != nil || c.MaxUnavailable != "25%" || c.Timeout != 270*time.Second {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	for k, v := range map[string]string{
		"NAMESPACE": "", "DAEMONSET": "a/b", "PROVIDER_NAME": "UPPER", "MAX_UNAVAILABLE": "0", "TIMEOUT_SECONDS": "-5",
	} {
		vals := map[string]string{k: v}
		for bk, bv := range base {
			if bk != k {
				vals[bk] = bv
			}
		}
		if _, err := loadConfig(env(vals)); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}

// TestRun_RetriesAPIErrors: a failing list is retried until the deadline.
func TestRun_RetriesAPIErrors(t *testing.T) {
	f := &fakeAPI{ds: newDaemonSet(1), listErr: errors.New("connection refused")}
	f.pods = []pod{podOn("a", "install", 0, nil)}
	cfg := testConfig()
	cfg.Timeout = 100 * time.Millisecond
	var out bytes.Buffer
	if err := run(context.Background(), f, cfg, &out); err == nil || !strings.Contains(out.String(), "connection refused; trying again") {
		t.Fatalf("got %v\n%s", err, out.String())
	}
}
