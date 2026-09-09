package orchestrate

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func managed(name string, conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "media",
			Labels: map[string]string{LabelManagedBy: ManagedBy},
		},
		Status: batchv1.JobStatus{Conditions: conditions},
	}
}

func condition(t batchv1.JobConditionType, s corev1.ConditionStatus) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: s}
}

func TestActiveJobsCountsOnlyWhatOccupiesASlot(t *testing.T) {
	foreign := managed("someone-elses")
	foreign.Labels = nil

	k := cluster{cs: fake.NewClientset(
		managed("running"),
		managed("still-going", condition(batchv1.JobComplete, corev1.ConditionFalse)),
		managed("done", condition(batchv1.JobComplete, corev1.ConditionTrue)),
		managed("failed", condition(batchv1.JobFailed, corev1.ConditionTrue)),
		managed("suspended", condition(batchv1.JobSuspended, corev1.ConditionTrue)),
		foreign,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: "elsewhere", Namespace: "other",
			Labels: map[string]string{LabelManagedBy: ManagedBy},
		}},
	)}

	n, err := k.ActiveJobs(context.Background(), "media")
	if err != nil {
		t.Fatal(err)
	}
	// running, still-going and suspended: a Job that has not reached a verdict
	// still holds its slot, and one owned by nobody else's controller is not
	// ours to count.
	if n != 3 {
		t.Errorf("active = %d, want 3", n)
	}
}

func TestCreateJobTreatsAnExistingNameAsQueued(t *testing.T) {
	k := cluster{cs: fake.NewClientset()}
	job := managed("conform-abc")

	created, err := k.CreateJob(context.Background(), job)
	if err != nil || !created {
		t.Fatalf("first create = %v, %v; want true, nil", created, err)
	}
	created, err = k.CreateJob(context.Background(), job)
	if err != nil {
		t.Fatalf("a name collision is the dedupe, not a fault: %v", err)
	}
	if created {
		t.Error("created = true for a Job that already exists")
	}
}

func TestCreateJobReportsARealFailure(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	created, err := cluster{cs: cs}.CreateJob(context.Background(), managed("conform-abc"))
	if err == nil {
		t.Error("a create that failed for any other reason must not be swallowed")
	}
	if created {
		t.Error("created = true after a failed create")
	}
}

func TestJobNameIsALegalObjectName(t *testing.T) {
	f := request().File
	f.Path = "/media/TV Shows/Detectorists (2014)/S01E01 — Pilot.mkv"

	if errs := validation.IsDNS1123Subdomain(JobName(f)); len(errs) > 0 {
		t.Errorf("JobName(%q) = %q: %v", f.Path, JobName(f), errs)
	}
}

// The end-to-end shape of a pass: read a PodTemplate from the cluster, create
// the Jobs, and — on the pass after — find them already there.
func TestDispatchThroughAClusterIsIdempotent(t *testing.T) {
	cs := fake.NewClientset(template(worker()))
	// Room to spare, so the second pass reaches the create and the collision
	// there is what reports the file as queued.
	room := opts()
	room.MaxActive = 4
	o := &Orchestrator{Kube: cluster{cs: cs}, Opts: room}
	reqs := requests(2)

	first, err := o.Dispatch(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range first {
		if d.Outcome != OutcomeCreated {
			t.Fatalf("%s: outcome = %q, want created", d.Path, d.Outcome)
		}
	}

	jobs, err := cs.BatchV1().Jobs("media").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("cluster holds %d jobs, want 2", len(jobs.Items))
	}
	got := jobs.Items[0].Spec.Template.Spec.Containers[0].Args
	if got[len(got)-1] != jobs.Items[0].Annotations[AnnotationPath] {
		t.Errorf("args = %v, want the annotated path appended", got)
	}

	second, err := o.Dispatch(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range second {
		if d.Outcome != OutcomeQueued {
			t.Errorf("%s: outcome = %q on the second pass, want queued", d.Path, d.Outcome)
		}
	}
	jobs, err = cs.BatchV1().Jobs("media").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Errorf("second pass left %d jobs, want the same 2", len(jobs.Items))
	}
}

// The two live Jobs from the pass above fill the cap, so the next pass over a
// library that has grown defers rather than piling more on.
func TestDispatchDefersOnceTheClusterIsFull(t *testing.T) {
	cs := fake.NewClientset(template(worker()), managed("conform-1"), managed("conform-2"))
	o := &Orchestrator{Kube: cluster{cs: cs}, Opts: opts()}

	out, err := o.Dispatch(context.Background(), requests(1))
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Outcome != OutcomeDeferred {
		t.Errorf("outcome = %q with the cap already met, want deferred", out[0].Outcome)
	}
}

// A file already in flight is reported deferred, not queued, once the cap is
// met: the Jobs holding those slots are the very files being re-derived, and
// the cap is checked before the create that would have recognised them. Only
// the printed verdict differs — no second Job is created either way.
func TestDispatchDefersAFileAlreadyInFlight(t *testing.T) {
	cs := fake.NewClientset(template(worker()))
	o := &Orchestrator{Kube: cluster{cs: cs}, Opts: opts()}
	reqs := requests(2)

	if _, err := o.Dispatch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	out, err := o.Dispatch(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range out {
		if d.Outcome != OutcomeDeferred {
			t.Errorf("%s: outcome = %q, want deferred", d.Path, d.Outcome)
		}
	}
	jobs, err := cs.BatchV1().Jobs("media").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Errorf("cluster holds %d jobs, want the original 2", len(jobs.Items))
	}
}
