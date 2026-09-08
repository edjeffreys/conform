package orchestrate

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

func opts() config.Orchestrator {
	ttl, backoff := int32(60), int32(2)
	return config.Orchestrator{
		Namespace: "media", MaxActive: 2,
		TTLSecondsAfterFinished: &ttl, BackoffLimit: &backoff,
	}
}

func template(containers ...corev1.Container) *corev1.PodTemplate {
	return &corev1.PodTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "conform-qsv", Namespace: "media"},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"from": "template"}},
			Spec: corev1.PodSpec{
				NodeSelector:  map[string]string{"intel.feature.node.kubernetes.io/gpu": "true"},
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers:    containers,
			},
		},
	}
}

func worker() corev1.Container {
	return corev1.Container{Name: "conform", Args: []string{"apply", "-config", "/config/conform.yaml"}}
}

func request() Request {
	return Request{
		File:    &media.File{Path: "/media/films/A Film.mkv", Size: 100, ModTime: time.Unix(1, 0)},
		Profile: config.Profile{Job: config.JobSpec{PodTemplate: "conform-qsv", Container: "conform"}},
		Library: "films",
	}
}

func TestBuildJobAppendsThePathAndCarriesPlacementThrough(t *testing.T) {
	job, err := BuildJob(template(worker(), corev1.Container{Name: "sidecar"}), request(), opts())
	if err != nil {
		t.Fatal(err)
	}

	got := job.Spec.Template.Spec.Containers[0].Args
	want := []string{"apply", "-config", "/config/conform.yaml", "/media/films/A Film.mkv"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
	if len(job.Spec.Template.Spec.Containers[1].Args) != 0 {
		t.Error("the sidecar's args were touched")
	}

	if job.Spec.Template.Spec.NodeSelector["intel.feature.node.kubernetes.io/gpu"] != "true" {
		t.Error("the template's node selector was dropped")
	}
	if job.Spec.Template.Labels["from"] != "template" {
		t.Error("the template's own labels were dropped")
	}
	if job.Namespace != "media" || *job.Spec.BackoffLimit != 2 || *job.Spec.TTLSecondsAfterFinished != 60 {
		t.Errorf("job metadata not taken from the config: %+v", job.Spec)
	}
	if job.Labels[LabelManagedBy] != ManagedBy || job.Labels[LabelLibrary] != "films" {
		t.Errorf("labels = %v", job.Labels)
	}
	if job.Annotations[AnnotationPath] != "/media/films/A Film.mkv" {
		t.Errorf("annotations = %v", job.Annotations)
	}
}

func TestBuildJobDropsALibraryNameThatCannotBeALabel(t *testing.T) {
	req := request()
	req.Library = "TV Shows"

	job, err := BuildJob(template(worker()), req, opts())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := job.Labels[LabelLibrary]; ok {
		t.Errorf("label = %q; a space is not a legal label value and the create would fail", v)
	}
	if job.Labels[LabelManagedBy] != ManagedBy {
		t.Error("managed-by is load-bearing and must survive")
	}
	if job.Annotations[AnnotationLibrary] != "TV Shows" {
		t.Errorf("annotation = %q, want the name unchanged", job.Annotations[AnnotationLibrary])
	}
}

func TestBuildJobDoesNotMutateTheTemplate(t *testing.T) {
	tmpl := template(worker())
	if _, err := BuildJob(tmpl, request(), opts()); err != nil {
		t.Fatal(err)
	}
	if n := len(tmpl.Template.Spec.Containers[0].Args); n != 3 {
		t.Errorf("the cached template gained an arg: %d, want 3", n)
	}
}

func TestBuildJobOverridesRestartAlways(t *testing.T) {
	job, err := BuildJob(template(worker()), request(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never — a Job cannot take Always",
			job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestBuildJobRejectsAnUnusableTemplate(t *testing.T) {
	if _, err := BuildJob(template(corev1.Container{Name: "other", Args: []string{"apply"}}), request(), opts()); err == nil {
		t.Error("a template without the named container should fail, not fall back to the first")
	}
	if _, err := BuildJob(template(corev1.Container{Name: "conform"}), request(), opts()); err == nil {
		t.Error("a container with no args should fail: the path would become the whole command")
	}
}

func TestJobNameFollowsTheFileNotThePath(t *testing.T) {
	f := &media.File{Path: "/media/a.mkv", Size: 100, ModTime: time.Unix(1, 0)}
	same := &media.File{Path: "/media/a.mkv", Size: 100, ModTime: time.Unix(1, 0)}
	replaced := &media.File{Path: "/media/a.mkv", Size: 200, ModTime: time.Unix(2, 0)}

	if JobName(f) != JobName(same) {
		t.Error("the same file must name the same Job; that is the already-queued check")
	}
	if JobName(f) == JobName(replaced) {
		t.Error("a replaced file must name a new Job, or a finished Job inside its TTL blocks it")
	}
}

type fakeKube struct {
	active    int
	exists    map[string]bool
	created   []*batchv1.Job
	templates int
}

func (k *fakeKube) PodTemplate(_ context.Context, _, name string) (*corev1.PodTemplate, error) {
	k.templates++
	return template(worker()), nil
}

func (k *fakeKube) ActiveJobs(context.Context, string) (int, error) { return k.active, nil }

func (k *fakeKube) CreateJob(_ context.Context, job *batchv1.Job) (bool, error) {
	if k.exists[job.Name] {
		return false, nil
	}
	k.created = append(k.created, job)
	return true, nil
}

func requests(n int) []Request {
	var out []Request
	for i := range n {
		r := request()
		r.File = &media.File{Path: string(rune('a'+i)) + ".mkv", Size: int64(i), ModTime: time.Unix(1, 0)}
		out = append(out, r)
	}
	return out
}

func TestDispatchStopsAtTheCap(t *testing.T) {
	k := &fakeKube{active: 1}
	o := &Orchestrator{Kube: k, Opts: opts()}

	out, err := o.Dispatch(context.Background(), requests(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 1 {
		t.Fatalf("created %d jobs with one slot free, want 1", len(k.created))
	}
	if out[0].Outcome != OutcomeCreated || out[1].Outcome != OutcomeDeferred || out[2].Outcome != OutcomeDeferred {
		t.Errorf("outcomes = %v %v %v", out[0].Outcome, out[1].Outcome, out[2].Outcome)
	}
	if k.templates != 1 {
		t.Errorf("fetched the same PodTemplate %d times, want 1", k.templates)
	}
}

func TestDispatchTreatsAnExistingJobAsQueued(t *testing.T) {
	reqs := requests(2)
	k := &fakeKube{exists: map[string]bool{JobName(reqs[0].File): true}}
	o := &Orchestrator{Kube: k, Opts: opts()}

	out, err := o.Dispatch(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Outcome != OutcomeQueued {
		t.Errorf("outcome = %q, want queued: the file is already in flight", out[0].Outcome)
	}
	// The existing Job was already counted by ActiveJobs, so finding it must
	// not spend a second slot.
	if out[1].Outcome != OutcomeCreated {
		t.Errorf("second outcome = %q, want created", out[1].Outcome)
	}
}

func TestDispatchCreatesNothingWhenDryRun(t *testing.T) {
	k := &fakeKube{}
	o := &Orchestrator{Kube: k, Opts: opts(), DryRun: true}

	out, err := o.Dispatch(context.Background(), requests(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 0 {
		t.Error("dry run created a Job")
	}
	if out[0].Job == nil {
		t.Error("dry run should still build the Job it would create")
	}
}

func TestDispatchRefusesAProfileWithNoPodTemplate(t *testing.T) {
	reqs := requests(1)
	reqs[0].Profile.Job.PodTemplate = ""
	o := &Orchestrator{Kube: &fakeKube{}, Opts: opts()}

	if _, err := o.Dispatch(context.Background(), reqs); err == nil {
		t.Error("a profile with no podTemplate should fail rather than be skipped silently")
	}
}
