// Package orchestrate turns a plan into Kubernetes Jobs, one per file.
//
// There is no queue and no job database: the cluster holds both. A Job's name
// is derived from the file it processes, so creating one that already exists
// is how a later pass discovers the file is already in flight.
package orchestrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelLibrary   = "conform.jeffreys.email/library"
	LabelProfile   = "conform.jeffreys.email/profile"
	// The path is an annotation, not a label: it exceeds 63 characters and
	// contains separators no label value may carry.
	AnnotationPath    = "conform.jeffreys.email/path"
	AnnotationLibrary = "conform.jeffreys.email/library"

	ManagedBy = "conform"
)

// Kube is the slice of the API conform uses. An interface so a Job can be
// built and previewed without a cluster.
type Kube interface {
	PodTemplate(ctx context.Context, namespace, name string) (*corev1.PodTemplate, error)
	ActiveJobs(ctx context.Context, namespace string) (int, error)
	// CreateJob reports false when a Job of that name already exists, which is
	// not an error: it means the file is already queued.
	CreateJob(ctx context.Context, job *batchv1.Job) (bool, error)
}

type Request struct {
	File    *media.File
	Profile config.Profile
	Library string
}

type Outcome string

const (
	OutcomeCreated Outcome = "created"
	// Queued is a Job that already existed, so the file is in flight from an
	// earlier pass. That collision is the whole of the deduplication.
	OutcomeQueued Outcome = "queued"
	// Deferred is over the cap, and is recorded nowhere: the next pass derives
	// it again from the library.
	OutcomeDeferred Outcome = "deferred"
)

type Dispatched struct {
	Path string
	// Nil when the file was deferred, which is the one outcome with no Job.
	Job     *batchv1.Job
	Outcome Outcome
}

func (d Dispatched) Name() string {
	if d.Job == nil {
		return ""
	}
	return d.Job.Name
}

// JobName is derived from the file rather than from a counter, which is what
// lets creation itself be the "already queued" check. Size and mtime are in
// the hash alongside the path, so a file replaced while its finished Job is
// still inside its TTL gets a new name instead of colliding with the old one.
func JobName(f *media.File) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", f.Path, f.Size, f.ModTime.UnixNano()))
	return "conform-" + hex.EncodeToString(sum[:8])
}

// BuildJob copies the profile's PodTemplate and appends the file's path to the
// named container's arguments. Everything else in the template — devices, node
// selectors, tolerations, mounts — is carried through uninterpreted, so a
// profile can require hardware conform knows nothing about.
func BuildJob(tmpl *corev1.PodTemplate, req Request, o config.Orchestrator) (*batchv1.Job, error) {
	spec := *tmpl.Template.DeepCopy()

	name := req.Profile.Job.Container
	i := slices.IndexFunc(spec.Spec.Containers, func(c corev1.Container) bool { return c.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("PodTemplate %q has no container named %q", tmpl.Name, name)
	}
	// The path is appended rather than replacing the args, so the subcommand
	// and flags stay in the template with the rest of the placement data. An
	// empty list would leave the path as the whole command line.
	if len(spec.Spec.Containers[i].Args) == 0 {
		return nil, fmt.Errorf("container %q declares no args; conform appends the file path to them", name)
	}
	spec.Spec.Containers[i].Args = append(spec.Spec.Containers[i].Args, req.File.Path)

	// A Job cannot accept the Always its PodTemplate defaults to, and a
	// restart in place would re-run a transcode that already reached a verdict.
	if spec.Spec.RestartPolicy == "" || spec.Spec.RestartPolicy == corev1.RestartPolicyAlways {
		spec.Spec.RestartPolicy = corev1.RestartPolicyNever
	}

	labels := map[string]string{LabelManagedBy: ManagedBy}
	// A library name is free text — "TV Shows" is not a legal label value, and
	// the create would fail on it. Only managed-by is load-bearing, so the
	// library is dropped from the labels when it cannot be one and kept in the
	// annotation either way.
	if len(validation.IsValidLabelValue(req.Library)) == 0 {
		labels[LabelLibrary] = req.Library
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	for k, v := range labels {
		spec.Labels[k] = v
	}

	return &batchv1.Job{
		// Set so a dry run prints something kubectl will accept; the API
		// server fills it in either way.
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(req.File),
			Namespace: o.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				AnnotationPath:    req.File.Path,
				AnnotationLibrary: req.Library,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            o.BackoffLimit,
			TTLSecondsAfterFinished: o.TTLSecondsAfterFinished,
			Template:                spec,
		},
	}, nil
}

type Orchestrator struct {
	Kube Kube
	Opts config.Orchestrator
	// DryRun builds every Job and creates none.
	DryRun bool
}

// Dispatch creates a Job per request, up to the cap on Jobs in flight. What is
// left over is not tracked: the next pass re-derives it from the library, the
// same way it derived this one.
func (o *Orchestrator) Dispatch(ctx context.Context, reqs []Request) ([]Dispatched, error) {
	var out []Dispatched
	if len(reqs) == 0 {
		return nil, nil
	}

	room := o.Opts.MaxActive
	if !o.DryRun {
		active, err := o.Kube.ActiveJobs(ctx, o.Opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("count jobs in flight: %w", err)
		}
		room = o.Opts.MaxActive - active
	}

	templates := map[string]*corev1.PodTemplate{}
	for _, req := range reqs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if room <= 0 {
			out = append(out, Dispatched{Path: req.File.Path, Outcome: OutcomeDeferred})
			continue
		}

		name := req.Profile.Job.PodTemplate
		if name == "" {
			return out, fmt.Errorf("%s: its profile names no podTemplate", req.File.Path)
		}
		tmpl, ok := templates[name]
		if !ok {
			fetched, err := o.Kube.PodTemplate(ctx, o.Opts.Namespace, name)
			if err != nil {
				return out, fmt.Errorf("podtemplate %q: %w", name, err)
			}
			templates[name] = fetched
			tmpl = fetched
		}

		job, err := BuildJob(tmpl, req, o.Opts)
		if err != nil {
			return out, err
		}
		if o.DryRun {
			out = append(out, Dispatched{Path: req.File.Path, Job: job, Outcome: OutcomeCreated})
			room--
			continue
		}

		created, err := o.Kube.CreateJob(ctx, job)
		if err != nil {
			return out, fmt.Errorf("create job for %s: %w", req.File.Path, err)
		}
		outcome := OutcomeQueued
		if created {
			outcome = OutcomeCreated
			room--
		}
		out = append(out, Dispatched{Path: req.File.Path, Job: job, Outcome: outcome})
	}
	return out, nil
}
