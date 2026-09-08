package orchestrate

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type cluster struct{ cs kubernetes.Interface }

// NewKube falls back to the in-cluster service account, so the orchestrator
// running as a Pod needs no configuration of its own; an explicit kubeconfig
// is what makes it runnable from a workstation against the same config.
func NewKube(kubeconfig string) (Kube, error) {
	cfg, err := rest.InClusterConfig()
	if kubeconfig != "" || err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			rules.ExplicitPath = kubeconfig
		}
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no cluster credentials: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return cluster{cs: cs}, nil
}

func (k cluster) PodTemplate(ctx context.Context, namespace, name string) (*corev1.PodTemplate, error) {
	return k.cs.CoreV1().PodTemplates(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k cluster) ActiveJobs(ctx context.Context, namespace string) (int, error) {
	list, err := k.cs.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedBy,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		if !finished(&list.Items[i]) {
			n++
		}
	}
	return n, nil
}

func (k cluster) CreateJob(ctx context.Context, job *batchv1.Job) (bool, error) {
	_, err := k.cs.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	return err == nil, err
}

// A Job with neither condition is running or still waiting to be scheduled,
// and either way occupies a slot.
func finished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}
