package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation/detect"
)

// fakeTool is a read-only tool whose behavior is a closure over decoded args.
type fakeTool struct {
	name     string
	mutating bool
	fn       func(args map[string]any) (tools.Result, error)

	mu    sync.Mutex
	calls []string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake " + f.name }
func (f *fakeTool) Schema() []byte      { return []byte(`{}`) }
func (f *fakeTool) Mutating() bool      { return f.mutating }

func (f *fakeTool) Call(_ context.Context, args []byte) (tools.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, string(args))
	f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return tools.Result{}, fmt.Errorf("bad args: %w", err)
	}
	return f.fn(m)
}

func (f *fakeTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// memStore is an in-memory EvidenceStore; failAfter > 0 makes Add fail once
// that many rows are stored.
type memStore struct {
	rows      []domain.Evidence
	failAfter int
}

func (m *memStore) Add(_ context.Context, e domain.Evidence) error {
	if m.failAfter > 0 && len(m.rows) >= m.failAfter {
		return errors.New("disk full")
	}
	m.rows = append(m.rows, e)
	return nil
}

func (m *memStore) List(_ context.Context, id string) ([]domain.Evidence, error) {
	var out []domain.Evidence
	for _, r := range m.rows {
		if r.InvestigationID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }

func (m *memStore) ids() []string {
	out := make([]string, len(m.rows))
	for i, r := range m.rows {
		out[i] = r.ID
	}
	return out
}

func (m *memStore) has(id string) bool {
	for _, r := range m.rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func oomPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", Ready: true},
			{
				Name:         "app",
				RestartCount: 3,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137,
				}},
			},
		}},
	}
}

type fixture struct {
	get, list, describe, events, logs, rollout *fakeTool
	reg                                        *tools.Registry
}

func newFixture(extra ...tools.Tool) *fixture {
	pod := oomPod()
	f := &fixture{
		get: &fakeTool{name: "k8s_get", fn: func(a map[string]any) (tools.Result, error) {
			if a["kind"] == "node" {
				return tools.Result{
					Summary: "node node-a Ready",
					Data:    &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
				}, nil
			}
			return tools.Result{
				Summary: "deployment prod/api 1/1",
				Data: &kube.Workload{
					Kind: "deployment", Namespace: "prod", Name: "api",
					Selector: map[string]string{"app": "api"}, Desired: 1, Ready: 1,
				},
			}, nil
		}},
		list: &fakeTool{name: "k8s_list_pods", fn: func(map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "api-1 Running 1/2 restarts=3 node-a", Data: []corev1.Pod{pod}}, nil
		}},
		describe: &fakeTool{name: "k8s_describe", fn: func(map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "pod api-1 lastState OOMKilled exit 137", Data: &pod}, nil
		}},
		events: &fakeTool{name: "k8s_events", fn: func(map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "BackOff restarting", Data: []corev1.Event{{Reason: "BackOff"}}}, nil
		}},
		logs: &fakeTool{name: "pod_logs", fn: func(map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "fatal: out of memory", Data: "fatal: out of memory"}, nil
		}},
		rollout: &fakeTool{name: "rollout_history", fn: func(map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "rev 2 api:v2", Data: []kube.Revision{{Name: "api-2", Revision: 2}}}, nil
		}},
	}
	all := []tools.Tool{f.get, f.list, f.describe, f.events, f.logs, f.rollout}
	all = append(all, extra...)
	f.reg = tools.NewRegistry(false, all...)
	return f
}

func opts() Options {
	return Options{Cluster: "prod-eu", Kind: "deploy", Namespace: "prod", Name: "api"}
}

// stubDetectors replaces the detector seam for the duration of a test.
func stubDetectors(t *testing.T, fn func(detect.Snapshot) []domain.Finding) {
	t.Helper()
	old := runDetectors
	runDetectors = fn
	t.Cleanup(func() { runDetectors = old })
}

func TestDiagnoseHappyPath(t *testing.T) {
	var snap detect.Snapshot
	stubDetectors(t, func(s detect.Snapshot) []domain.Finding {
		snap = s
		return []domain.Finding{{
			Detector: "oomkilled", Resource: "pod/api-1", Summary: "oom",
			EvidenceIDs: []string{s.Pods[0].EvidenceID, s.Pods[0].LogsEvidence},
		}}
	})
	f := newFixture()
	store := &memStore{}

	inv, err := Diagnose(context.Background(), f.reg, store, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	wantTools := []string{"k8s_get", "k8s_list_pods", "k8s_describe", "k8s_events", "pod_logs", "k8s_get", "rollout_history"}
	if len(store.rows) != len(wantTools) {
		t.Fatalf("stored %d rows, want %d: %+v", len(store.rows), len(wantTools), store.rows)
	}
	for i, r := range store.rows {
		if want := fmt.Sprintf("E%d", i+1); r.ID != want {
			t.Errorf("row %d ID = %q, want %q", i, r.ID, want)
		}
		if r.Tool != wantTools[i] {
			t.Errorf("row %d tool = %q, want %q", i, r.Tool, wantTools[i])
		}
		if r.InvestigationID != inv.ID {
			t.Errorf("row %d investigation = %q, want %q", i, r.InvestigationID, inv.ID)
		}
		sum := sha256.Sum256([]byte(r.Excerpt))
		if r.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("row %d sha256 mismatch", i)
		}
		if r.TS.IsZero() || r.TS.Location().String() != "UTC" {
			t.Errorf("row %d TS = %v, want UTC", i, r.TS)
		}
		if !json.Valid([]byte(r.Args)) {
			t.Errorf("row %d args not JSON: %q", i, r.Args)
		}
	}

	if !regexp.MustCompile(`^inv-[0-9a-f]{8}$`).MatchString(inv.ID) {
		t.Errorf("ID = %q", inv.ID)
	}
	if inv.Cluster != "prod-eu" || inv.Trigger.Type != domain.TriggerAsk || inv.Trigger.Name != "deployment/prod/api" {
		t.Errorf("cluster/trigger = %q %+v", inv.Cluster, inv.Trigger)
	}
	if inv.ToolCalls != 7 {
		t.Errorf("ToolCalls = %d, want 7", inv.ToolCalls)
	}
	if inv.StartedAt.IsZero() || inv.Duration < 0 {
		t.Errorf("StartedAt/Duration = %v %v", inv.StartedAt, inv.Duration)
	}
	if want := "1 finding(s) for deployment/prod/api; 0 unknown(s)"; inv.Summary != want {
		t.Errorf("Summary = %q, want %q", inv.Summary, want)
	}
	if len(inv.Findings) != 1 {
		t.Fatalf("findings = %+v", inv.Findings)
	}
	for _, id := range inv.Findings[0].EvidenceIDs {
		if !store.has(id) {
			t.Errorf("finding cites %q which is not in the store (%v)", id, store.ids())
		}
	}

	// Snapshot wiring.
	if snap.Workload == nil || snap.WorkloadEvidence != "E1" {
		t.Errorf("workload = %v evidence %q", snap.Workload, snap.WorkloadEvidence)
	}
	if len(snap.Pods) != 1 {
		t.Fatalf("pods = %d", len(snap.Pods))
	}
	p := snap.Pods[0]
	if p.EvidenceID != "E3" || p.EventsEvidence != "E4" || p.LogsEvidence != "E5" {
		t.Errorf("pod evidence = %q %q %q", p.EvidenceID, p.EventsEvidence, p.LogsEvidence)
	}
	if p.Pod == nil || len(p.Events) != 1 || p.Logs != "fatal: out of memory" {
		t.Errorf("pod obs = %+v", p)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].EvidenceID != "E6" || snap.Nodes[0].Node == nil {
		t.Errorf("nodes = %+v", snap.Nodes)
	}
	if len(snap.Revisions) != 1 || snap.RevisionsEvidence != "E7" {
		t.Errorf("revisions = %+v %q", snap.Revisions, snap.RevisionsEvidence)
	}
}

func TestDiagnoseBuildsArgs(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	o := opts()
	o.TailLines = 50
	if _, err := Diagnose(context.Background(), f.reg, &memStore{}, o); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := decode(f.get.calls[0]); m["kind"] != "deployment" || m["namespace"] != "prod" || m["name"] != "api" {
		t.Errorf("workload args = %v", m)
	}
	if m := decode(f.get.calls[1]); m["kind"] != "node" || m["name"] != "node-a" {
		t.Errorf("node args = %v", m)
	}
	if m := decode(f.list.calls[0]); m["namespace"] != "prod" || fmt.Sprint(m["selector"]) != "map[app:api]" {
		t.Errorf("list args = %v", m)
	}
	if m := decode(f.describe.calls[0]); m["pod"] != "api-1" {
		t.Errorf("describe args = %v", m)
	}
	if m := decode(f.events.calls[0]); m["name"] != "api-1" {
		t.Errorf("events args = %v", m)
	}
	m := decode(f.logs.calls[0])
	if m["container"] != "app" || m["previous"] != true || m["tail_lines"] != float64(50) {
		t.Errorf("logs args = %v", m)
	}
	if m := decode(f.rollout.calls[0]); m["name"] != "api" || m["namespace"] != "prod" {
		t.Errorf("rollout args = %v", m)
	}
}

func TestDiagnoseCurrentLogsWithoutRestart(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	healthy := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}}},
	}
	f.list.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: []corev1.Pod{healthy}}, nil
	}
	f.describe.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: &healthy}, nil
	}
	if _, err := Diagnose(context.Background(), f.reg, &memStore{}, opts()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(f.logs.calls[0]), &m)
	if m["previous"] == true {
		t.Errorf("previous = true for a pod with no restarts: %v", m)
	}
}

func TestDiagnoseLogFailureIsUnknown(t *testing.T) {
	var snap detect.Snapshot
	stubDetectors(t, func(s detect.Snapshot) []domain.Finding { snap = s; return nil })
	f := newFixture()
	f.logs.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{}, errors.New("container not found, token=abc123secret")
	}
	store := &memStore{}

	inv, err := Diagnose(context.Background(), f.reg, store, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(inv.Unknowns) != 1 {
		t.Fatalf("unknowns = %v", inv.Unknowns)
	}
	if !strings.Contains(inv.Unknowns[0], "pod_logs") || !strings.Contains(inv.Unknowns[0], "api-1") {
		t.Errorf("unknown lacks context: %q", inv.Unknowns[0])
	}
	if strings.Contains(inv.Unknowns[0], "abc123secret") {
		t.Errorf("unknown not redacted: %q", inv.Unknowns[0])
	}
	if inv.ToolCalls != 7 {
		t.Errorf("ToolCalls = %d, want 7 (failed attempts count)", inv.ToolCalls)
	}
	if got := snap.Pods[0].LogsEvidence; got != "" {
		t.Errorf("LogsEvidence = %q, want empty for a failed call", got)
	}
	for _, r := range store.rows {
		if r.Tool == "pod_logs" {
			t.Errorf("failed call stored: %+v", r)
		}
	}
	if want := "0 finding(s) for deployment/prod/api; 1 unknown(s)"; inv.Summary != want {
		t.Errorf("Summary = %q, want %q", inv.Summary, want)
	}
	// Later calls keep sequential IDs: nothing was skipped in the store.
	for i, r := range store.rows {
		if want := fmt.Sprintf("E%d", i+1); r.ID != want {
			t.Errorf("row %d ID = %q, want %q", i, r.ID, want)
		}
	}
}

func TestDiagnoseUnknownsSorted(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	f.rollout.fn = func(map[string]any) (tools.Result, error) { return tools.Result{}, errors.New("forbidden") }
	f.events.fn = func(map[string]any) (tools.Result, error) { return tools.Result{}, errors.New("timeout") }
	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Unknowns) != 2 || inv.Unknowns[0] > inv.Unknowns[1] {
		t.Errorf("unknowns not sorted: %v", inv.Unknowns)
	}
}

func TestDiagnoseWrongDataTypeIsUnknown(t *testing.T) {
	var snap detect.Snapshot
	stubDetectors(t, func(s detect.Snapshot) []domain.Finding { snap = s; return nil })
	f := newFixture()
	f.logs.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "x", Data: 42}, nil
	}
	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(inv.Unknowns) != 1 || snap.Pods[0].LogsEvidence != "" {
		t.Errorf("unknowns=%v logsEvidence=%q", inv.Unknowns, snap.Pods[0].LogsEvidence)
	}
}

func TestDiagnoseWorkloadFailureIsFatal(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	f.get.fn = func(map[string]any) (tools.Result, error) { return tools.Result{}, errors.New("not found") }
	store := &memStore{}
	inv, err := Diagnose(context.Background(), f.reg, store, opts())
	if err == nil || inv != nil {
		t.Fatalf("inv=%v err=%v, want fatal error", inv, err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
	if f.list.callCount() != 0 {
		t.Error("pods listed after workload failure")
	}
}

func TestDiagnoseStoreErrorIsFatal(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	store := &memStore{failAfter: 2}
	inv, err := Diagnose(context.Background(), f.reg, store, opts())
	if err == nil || inv != nil {
		t.Fatalf("inv=%v err=%v, want fatal error", inv, err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v", err)
	}
	if f.events.callCount() != 0 {
		t.Error("kept collecting after the store failed")
	}
}

func TestDiagnoseMissingToolIsFatal(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	reg := tools.NewRegistry(false, f.get, f.list, f.describe, f.events, f.rollout) // no pod_logs
	if _, err := Diagnose(context.Background(), reg, &memStore{}, opts()); err == nil {
		t.Fatal("expected error for missing pod_logs")
	}
	if f.get.callCount() != 0 {
		t.Error("tools called before the registry was validated")
	}
}

func TestDiagnoseNonDeploymentSkipsRollout(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	f.get.fn = func(a map[string]any) (tools.Result, error) {
		if a["kind"] == "node" {
			return tools.Result{Summary: "node", Data: &corev1.Node{}}, nil
		}
		return tools.Result{Summary: "sts", Data: &kube.Workload{
			Kind: "statefulset", Namespace: "prod", Name: "db", Selector: map[string]string{"app": "db"},
		}}, nil
	}
	reg := tools.NewRegistry(false, f.get, f.list, f.describe, f.events, f.logs) // no rollout_history
	o := opts()
	o.Kind, o.Name = "sts", "db"
	inv, err := Diagnose(context.Background(), reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if inv.Trigger.Name != "statefulset/prod/db" {
		t.Errorf("trigger = %q", inv.Trigger.Name)
	}
}

func TestDiagnoseMutatingToolsUnreachable(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	del := &fakeTool{name: "k8s_delete_pod", mutating: true, fn: func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "deleted"}, nil
	}}
	f := newFixture(del)

	if _, ok := f.reg.Get("k8s_delete_pod"); ok {
		t.Fatal("mutating tool present in a registry built with allowMutating=false")
	}
	if _, err := Diagnose(context.Background(), f.reg, &memStore{}, opts()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if del.callCount() != 0 {
		t.Errorf("mutating tool called %d times", del.callCount())
	}

	// A required tool that is mutating is dropped, so Diagnose cannot run.
	f2 := newFixture()
	f2.logs.mutating = true
	reg := tools.NewRegistry(false, f2.get, f2.list, f2.describe, f2.events, f2.logs, f2.rollout)
	if _, err := Diagnose(context.Background(), reg, &memStore{}, opts()); err == nil {
		t.Fatal("expected error when pod_logs is mutating and dropped")
	}
	if f2.logs.callCount() != 0 {
		t.Error("dropped mutating tool was called")
	}
}

func TestDiagnoseHonoursCancellation(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	f.list.fn = func(map[string]any) (tools.Result, error) {
		cancel()
		return tools.Result{Summary: "pods", Data: []corev1.Pod{oomPod()}}, nil
	}
	inv, err := Diagnose(ctx, f.reg, &memStore{}, opts())
	if !errors.Is(err, context.Canceled) || inv != nil {
		t.Fatalf("inv=%v err=%v, want context.Canceled", inv, err)
	}
	if f.describe.callCount() != 0 {
		t.Error("kept calling tools after cancellation")
	}
}

func TestDiagnoseEmptySelectorSkipsPods(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	f.get.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "d", Data: &kube.Workload{Kind: "deployment", Namespace: "prod", Name: "api"}}, nil
	}
	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if f.list.callCount() != 0 || len(inv.Unknowns) == 0 {
		t.Errorf("list calls=%d unknowns=%v", f.list.callCount(), inv.Unknowns)
	}
}

const (
	dirtyBearer = "Bearer abcdef1234567890"
	dirtyPair   = "password=hunter2"
	dirtyANSI   = "\x1b[31m"
)

func assertClean(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{"hunter2", "abcdef1234567890"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %q", what, bad, s)
		}
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			t.Errorf("%s contains control rune %U: %q", what, r, s)
			return
		}
	}
}

func TestDiagnoseSanitizesRealDetectorFindings(t *testing.T) {
	stubDetectors(t, detect.Run) // restore the real detectors for this test
	f := newFixture()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app", Image: "reg.example/app:" + dirtyPair}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			Image: "reg.example/app:" + dirtyPair,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ImagePullBackOff",
				Message: dirtyANSI + "pull failed " + dirtyPair + " " + dirtyBearer + "\nretrying",
			}},
		}}},
	}
	f.list.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: []corev1.Pod{pod}}, nil
	}
	f.describe.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: &pod}, nil
	}
	f.events.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "events", Data: []corev1.Event{{
			Reason: "Failed", Message: dirtyANSI + dirtyPair + " " + dirtyBearer,
		}}}, nil
	}
	f.logs.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{}, errors.New("log fetch failed " + dirtyPair + " " + dirtyBearer + dirtyANSI + "\r\n")
	}

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var got *domain.Finding
	for i := range inv.Findings {
		if inv.Findings[i].Detector == "image_pull" {
			got = &inv.Findings[i]
		}
	}
	if got == nil {
		t.Fatalf("image_pull did not fire: %+v", inv.Findings)
	}
	if len(got.EvidenceIDs) != 1 || got.EvidenceIDs[0] != "E3" {
		t.Errorf("EvidenceIDs = %v, want [E3]", got.EvidenceIDs)
	}
	for _, fd := range inv.Findings {
		assertClean(t, "finding resource", fd.Resource)
		assertClean(t, "finding summary", fd.Summary)
		assertClean(t, "finding quote", fd.Quote)
	}
	if len(inv.Unknowns) == 0 {
		t.Fatal("expected an unknown for the failed log fetch")
	}
	for _, u := range inv.Unknowns {
		assertClean(t, "unknown", u)
	}
}

func TestDiagnoseSanitizesSeamFindings(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding {
		return []domain.Finding{{
			Detector:    "x",
			Resource:    "pod/a" + dirtyANSI + "b",
			Summary:     "line1\nline2\t" + dirtyPair + "\x00",
			Quote:       dirtyBearer + "\r\n" + dirtyANSI,
			EvidenceIDs: []string{"E1"},
		}}
	})
	inv, err := Diagnose(context.Background(), newFixture().reg, &memStore{}, opts())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	fd := inv.Findings[0]
	assertClean(t, "resource", fd.Resource)
	assertClean(t, "summary", fd.Summary)
	assertClean(t, "quote", fd.Quote)
	if !strings.Contains(fd.Summary, "line1 line2") {
		t.Errorf("newline/tab should become a space: %q", fd.Summary)
	}
	if fd.Detector != "x" || len(fd.EvidenceIDs) != 1 || fd.EvidenceIDs[0] != "E1" {
		t.Errorf("other fields changed: %+v", fd)
	}
}

func TestDiagnoseReadsFailingInitContainerLogs(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := newFixture()
	stuck := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "migrate-ok", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "migrate", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}},
		},
	}
	f.list.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: []corev1.Pod{stuck}}, nil
	}
	f.describe.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "api-1", Data: &stuck}, nil
	}
	if _, err := Diagnose(context.Background(), f.reg, &memStore{}, opts()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(f.logs.calls[0]), &m)
	if m["container"] != "migrate" || m["previous"] == true {
		t.Errorf("logs args = %v, want container=migrate, previous=false", m)
	}

	// A restarting init container reads the previous instance.
	stuck.Status.InitContainerStatuses[1].RestartCount = 4
	f2 := newFixture()
	f2.list.fn, f2.describe.fn = f.list.fn, f.describe.fn
	if _, err := Diagnose(context.Background(), f2.reg, &memStore{}, opts()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	m = nil
	_ = json.Unmarshal([]byte(f2.logs.calls[0]), &m)
	if m["container"] != "migrate" || m["previous"] != true {
		t.Errorf("logs args = %v, want container=migrate, previous=true", m)
	}
}

// capPod builds a pod for budget tests. ready and restarts drive the
// unhealthiest-first ordering.
func capPod(name, node string, ready bool, restarts int32) corev1.Pod {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	cs := corev1.ContainerStatus{Name: "app", Ready: ready, RestartCount: restarts}
	if !ready {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

// podsFixture serves pods (in the given order) through list and describe.
func podsFixture(pods []corev1.Pod) *fixture {
	f := newFixture()
	f.list.fn = func(map[string]any) (tools.Result, error) {
		return tools.Result{Summary: "pods", Data: append([]corev1.Pod(nil), pods...)}, nil
	}
	f.describe.fn = func(a map[string]any) (tools.Result, error) {
		for _, p := range pods {
			if p.Name == a["pod"] {
				return tools.Result{Summary: "pod " + p.Name, Data: &p}, nil
			}
		}
		return tools.Result{}, errors.New("no such pod")
	}
	return f
}

// calledPods returns the pod argument of every call, in order.
func calledPods(t *testing.T, ft *fakeTool) []string {
	t.Helper()
	var out []string
	for _, raw := range ft.calls {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(m["pod"]))
	}
	return out
}

func totalCalls(f *fixture) int {
	return f.get.callCount() + f.list.callCount() + f.describe.callCount() +
		f.events.callCount() + f.logs.callCount() + f.rollout.callCount()
}

func hasUnknown(inv *domain.Investigation, parts ...string) bool {
	for _, u := range inv.Unknowns {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(u, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestDiagnoseToolCallCapHonouredExactly(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	pods := []corev1.Pod{
		capPod("p1", "node-a", false, 3), capPod("p2", "node-a", false, 2),
		capPod("p3", "node-a", true, 0), capPod("p4", "node-a", true, 0), capPod("p5", "node-a", true, 0),
	}
	f := podsFixture(pods)
	o := opts()
	o.MaxToolCalls = 10

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	// get + list, two full pods (6), then the reserved node read and rollout.
	if inv.ToolCalls != 10 || totalCalls(f) != 10 {
		t.Errorf("ToolCalls = %d, tool invocations = %d, want exactly 10", inv.ToolCalls, totalCalls(f))
	}
	if f.get.callCount() != 2 || f.rollout.callCount() != 1 {
		t.Errorf("node/rollout reads were starved: get=%d rollout=%d", f.get.callCount(), f.rollout.callCount())
	}
	for _, name := range []string{"p3", "p4", "p5"} {
		want := fmt.Sprintf("pod %s not inspected: tool call cap (10) reached", name)
		if !hasUnknown(inv, want) {
			t.Errorf("missing unknown %q in %v", want, inv.Unknowns)
		}
	}
}

func TestDiagnoseToolCallCapNeverExceeded(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	for limit := 1; limit <= 20; limit++ {
		pods := []corev1.Pod{
			capPod("p1", "node-a", false, 3), capPod("p2", "node-b", false, 1),
			capPod("p3", "node-c", true, 0), capPod("p4", "node-a", true, 0),
		}
		f := podsFixture(pods)
		o := opts()
		o.MaxToolCalls = limit
		inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
		if err != nil {
			t.Fatalf("cap %d: %v", limit, err)
		}
		if inv.ToolCalls > limit || totalCalls(f) > limit {
			t.Errorf("cap %d: ToolCalls = %d, invocations = %d", limit, inv.ToolCalls, totalCalls(f))
		}
		if inv.ToolCalls != totalCalls(f) {
			t.Errorf("cap %d: ToolCalls = %d but %d tools were invoked", limit, inv.ToolCalls, totalCalls(f))
		}
	}
}

func TestDiagnoseToolCallCapPrefersUnhealthyPods(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	// The crashing pod is listed last, after three healthy ones.
	pods := []corev1.Pod{
		capPod("a-ok", "node-a", true, 0), capPod("b-ok", "node-a", true, 0),
		capPod("c-ok", "node-a", true, 0), capPod("z-crash", "node-a", false, 5),
	}
	f := podsFixture(pods)
	o := opts()
	o.MaxToolCalls = 7 // get, list, one pod (3), node, rollout

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if got := calledPods(t, f.describe); len(got) != 1 || got[0] != "z-crash" {
		t.Errorf("inspected %v, want only z-crash", got)
	}
	for _, name := range []string{"a-ok", "b-ok", "c-ok"} {
		if !hasUnknown(inv, "pod "+name+" not inspected") {
			t.Errorf("missing not-inspected unknown for %s: %v", name, inv.Unknowns)
		}
	}
	if hasUnknown(inv, "pod z-crash") {
		t.Errorf("inspected pod got a cap unknown: %v", inv.Unknowns)
	}
}

func TestDiagnoseToolCallCapEssentialsAlwaysRun(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	pods := []corev1.Pod{capPod("p1", "node-a", false, 3), capPod("p2", "node-a", true, 0)}
	f := podsFixture(pods)
	o := opts()
	o.MaxToolCalls = 2

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if f.get.callCount() != 1 || f.list.callCount() != 1 {
		t.Errorf("workload get = %d, pod list = %d, want 1 each", f.get.callCount(), f.list.callCount())
	}
	if f.describe.callCount()+f.events.callCount()+f.logs.callCount()+f.rollout.callCount() != 0 {
		t.Errorf("calls beyond the essentials with a cap of 2")
	}
	if inv.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", inv.ToolCalls)
	}
	for _, name := range []string{"p1", "p2"} {
		if !hasUnknown(inv, "pod "+name+" not inspected: tool call cap (2) reached") {
			t.Errorf("missing unknown for %s: %v", name, inv.Unknowns)
		}
	}
	if !hasUnknown(inv, toolRollout, "cap (2)") {
		t.Errorf("skipped rollout_history should be an Unknown: %v", inv.Unknowns)
	}
}

func TestDiagnoseToolCallCapOfOneStillReadsWorkload(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := podsFixture([]corev1.Pod{capPod("p1", "node-a", false, 1)})
	o := opts()
	o.MaxToolCalls = 1

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if f.get.callCount() != 1 || inv.ToolCalls != 1 || f.list.callCount() != 0 {
		t.Errorf("get=%d list=%d ToolCalls=%d", f.get.callCount(), f.list.callCount(), inv.ToolCalls)
	}
	if !hasUnknown(inv, toolListPods, "cap (1)") {
		t.Errorf("skipped pod list should be an Unknown: %v", inv.Unknowns)
	}
}

func TestDiagnoseToolCallCapPartialPodIsUnknown(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	f := podsFixture([]corev1.Pod{capPod("p1", "node-a", false, 3)})
	o := opts()
	o.MaxToolCalls = 5 // get, list, describe, node, rollout: no room for events or logs

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if f.describe.callCount() != 1 || f.events.callCount() != 0 || f.logs.callCount() != 0 {
		t.Errorf("describe=%d events=%d logs=%d", f.describe.callCount(), f.events.callCount(), f.logs.callCount())
	}
	if !hasUnknown(inv, "pod p1", "cap (5)") {
		t.Errorf("a partly inspected pod must be reported: %v", inv.Unknowns)
	}
	if inv.ToolCalls != 5 {
		t.Errorf("ToolCalls = %d, want 5", inv.ToolCalls)
	}
}

func TestDiagnoseToolCallCapReservesNodeReadsPerInspectedNode(t *testing.T) {
	var snap detect.Snapshot
	stubDetectors(t, func(s detect.Snapshot) []domain.Finding { snap = s; return nil })
	pods := []corev1.Pod{capPod("p1", "node-a", false, 3), capPod("p2", "node-b", false, 2)}
	f := podsFixture(pods)
	o := opts()
	o.MaxToolCalls = 11 // get, list, 2 pods (6), 2 nodes, rollout

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, o)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(snap.Nodes) != 2 || snap.RevisionsEvidence == "" {
		t.Errorf("nodes=%d revisions evidence=%q, want both node reads and the rollout", len(snap.Nodes), snap.RevisionsEvidence)
	}
	if inv.ToolCalls != 11 {
		t.Errorf("ToolCalls = %d, want 11", inv.ToolCalls)
	}
	if len(inv.Unknowns) != 0 {
		t.Errorf("unexpected unknowns: %v", inv.Unknowns)
	}
}

func TestDiagnoseZeroCapIsUnlimited(t *testing.T) {
	stubDetectors(t, func(detect.Snapshot) []domain.Finding { return nil })
	pods := []corev1.Pod{
		capPod("healthy", "node-a", true, 0), capPod("crash", "node-a", false, 4),
		capPod("other", "node-a", true, 0), capPod("last", "node-a", true, 0),
	}
	f := podsFixture(pods)

	inv, err := Diagnose(context.Background(), f.reg, &memStore{}, opts()) // MaxToolCalls: 0
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	// get + list + 4 pods * 3 + node + rollout.
	if inv.ToolCalls != 16 {
		t.Errorf("ToolCalls = %d, want 16", inv.ToolCalls)
	}
	want := []string{"healthy", "crash", "other", "last"}
	got := calledPods(t, f.describe)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("inspection order = %v, want list order %v when uncapped", got, want)
		}
	}
	if len(inv.Unknowns) != 0 {
		t.Errorf("unexpected unknowns: %v", inv.Unknowns)
	}
}

func TestPodsByUrgency(t *testing.T) {
	pending := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	succeeded := capPod("done", "n", true, 0)
	succeeded.Status.Phase = corev1.PodSucceeded
	restarted := capPod("restarted", "n", true, 2)
	notReady := capPod("not-ready", "n", false, 0)
	notReady.Status.ContainerStatuses[0].State = corev1.ContainerState{}
	crash := capPod("crash", "n", false, 9)

	in := []corev1.Pod{capPod("b-ok", "n", true, 0), succeeded, restarted, capPod("a-ok", "n", true, 0), notReady, pending, crash}
	got := podsByUrgency(in)
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	// Not ready first (restarts, then bad phase, then the rest), restarted next,
	// then name order among the healthy.
	want := []string{"crash", "pending", "not-ready", "restarted", "a-ok", "b-ok", "done"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", names, want)
	}
	if in[0].Name != "b-ok" {
		t.Error("podsByUrgency must not reorder its input")
	}
}
