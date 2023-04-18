package kubeci

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

var (
	paramCacheVolumeClaimName = "cacheVolumeClaimName"
	scopeBranch               = "branch"
	scopeProject              = "project"
	scopeNone                 = "none"
)

type K8sStorageManager struct {
	Namespace  string
	ManagedBy  string
	KubeClient kubernetes.Interface
}

func hash(strs ...string) string {
	h := sha1.New()

	allstrs := strings.Join(strs, "#")
	fmt.Fprint(h, allstrs)
	str := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	return "x" + str[0:8]
}

func (sm *K8sStorageManager) ensurePVC(
	wf *workflow.Workflow,
	org string,
	repo string,
	branch string,
	defaults CacheSpec) error {
	ctx := context.Background()
	scope := defaults.Scope
	if wfScope, ok := wf.Annotations[annCacheVolumeScope]; ok {
		scope = wfScope
	}
	if scope == scopeNone || scope == "" {
		return nil
	}
	if scope != scopeBranch && scope != scopeProject {
		return errors.New("scope should be either none, branch or project")
	}

	class := defaults.StorageClassName
	if wfClass, ok := wf.Annotations[annCacheVolumeStorageClassName]; ok {
		class = wfClass
	}

	resStr := defaults.Size
	if wfRes, ok := wf.Annotations[annCacheVolumeStorageSize]; ok {
		resStr = wfRes
	}

	if resStr == "" {
		return fmt.Errorf("cannot determine cache size, set a default or specify a %q annotation", annCacheVolumeStorageSize)
	}

	res, err := resource.ParseQuantity(resStr)
	if err != nil {
		return err
	}

	cacheHash := hash(org, repo)
	name := labelSafe("ci", scope, org, repo, cacheHash)
	if scope == scopeBranch {
		cacheHash = hash(org, repo, branch)
		name = labelSafe("ci", scope, org, repo, cacheHash)
	}

	ls := labels.Set(
		map[string]string{
			labelManagedBy: sm.ManagedBy,
			labelOrg:       labelSafe(org),
			labelRepo:      labelSafe(repo),
			labelScope:     labelSafe(scope),
		})

	if scope == scopeBranch {
		ls[labelCacheHash] = cacheHash
	}

	if wfVolName, ok := wf.Annotations[annCacheVolumeName]; ok {
		name = wfVolName
	}

	opt := metav1.GetOptions{}

	pv, err := sm.KubeClient.CoreV1().PersistentVolumeClaims(wf.Namespace).Get(ctx, name, opt)
	if err == nil {
		parms := wf.Spec.Arguments.Parameters
		wf.Spec.Arguments.Parameters = append(parms, workflow.Parameter{
			Name:  paramCacheVolumeClaimName,
			Value: workflow.AnyStringPtr(name),
		})

		return nil
	}

	if !k8errors.IsNotFound(err) {
		return err
	}

	spec := corev1.PersistentVolumeClaimSpec{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: res,
			},
		},
		AccessModes: []corev1.PersistentVolumeAccessMode{"ReadWriteOnce"},
	}

	if class != "" {
		spec.StorageClassName = &class
	}

	anns := map[string]string{
		annRepo: repo,
		annOrg:  org,
	}
	if scope == scopeBranch && branch != "" {
		anns[annBranch] = branch
	}

	pv = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   wf.Namespace,
			Labels:      ls,
			Annotations: anns,
		},
		Spec: spec,
	}

	_, err = sm.KubeClient.CoreV1().PersistentVolumeClaims(wf.Namespace).Create(ctx, pv, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	parms := wf.Spec.Arguments.Parameters
	wf.Spec.Arguments.Parameters = append(parms, workflow.Parameter{
		Name:  paramCacheVolumeClaimName,
		Value: workflow.AnyStringPtr(name),
	})

	return nil
}

func (sm *K8sStorageManager) getPodsForPVC(ctx context.Context, ns, pvcName string) ([]corev1.Pod, error) {
	// just in case
	if ns == "" || pvcName == "" {
		return nil, errors.New("pvc namespace or name cannot be empty strings")
	}
	nsPods, err := sm.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []corev1.Pod{}, err
	}

	var pods []corev1.Pod

	for _, pod := range nsPods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.VolumeSource.PersistentVolumeClaim != nil && volume.VolumeSource.PersistentVolumeClaim.ClaimName == pvcName {
				pods = append(pods, pod)
			}
		}
	}

	return pods, nil
}

// when a branch or repo gets deleted (or archived), we delete any pvc
// associated with it. If branch is not "", only that branch is deleted.
func (sm *K8sStorageManager) deletePVC(
	org string,
	repo string,
	branch string,
	action string) error {
	ctx := context.Background()

	log.Printf("clearing PVCs for %q/%q %q (action: %q)", org, repo, branch, action)

	ls := labels.Set(
		map[string]string{
			labelManagedBy: sm.ManagedBy,
			labelOrg:       labelSafe(org),
			labelRepo:      labelSafe(repo),
		})

	if branch != "" {
		ls[labelScope] = scopeBranch
	}

	sel := ls.AsSelector().String()
	if len(sel) == 0 {
		return fmt.Errorf("built nil selector! set = %#v", ls)
	}

	opt := metav1.ListOptions{
		LabelSelector: sel,
	}

	pvcs, err := sm.KubeClient.CoreV1().PersistentVolumeClaims(sm.Namespace).List(ctx, opt)
	if k8errors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	for _, pvc := range pvcs.Items {
		if branch != "" {
			b, ok := pvc.Annotations[annBranch]
			if !ok {
				continue
			}
			if b != branch {
				continue
			}
		}

		grace := int64(0)
		policy := metav1.DeletePropagationBackground
		dopts := metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			PropagationPolicy:  &policy,
		}
		err := sm.KubeClient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Delete(ctx, pvc.GetName(), dopts)
		if err != nil {
			log.Printf("failed to delete pvc %s/%s, %v", pvc.GetNamespace(), pvc.GetName(), err)
			continue
		}
		log.Printf("deleted pvc %s/%s for %s %s/%s",
			pvc.GetNamespace(),
			pvc.GetName(),
			action,
			org,
			repo)

		// We should probably cancel in-progress workflows

		// We need to delete any pods that have the PVC listed. Will not delete the
		// pvc until these pods are gone (even if they are not running)
		pods, err := sm.getPodsForPVC(ctx, pvc.GetNamespace(), pvc.GetName())
		if err != nil {
			log.Printf("failed to list pods for pvc %s/%s, %v", pvc.GetNamespace(), pvc.GetName(), err)
			continue
		}
		for _, p := range pods {
			err := sm.KubeClient.CoreV1().Pods(p.GetNamespace()).Delete(ctx, p.GetName(), dopts)
			if err != nil {
				log.Printf("failed to delete pod %s/%s using pvc %s/%s, %v", p.GetNamespace(), p.GetName(), pvc.GetNamespace(), pvc.GetName(), err)
				continue
			}
			log.Printf("deleted pod %s/%s using pvc %s/%s",
				p.GetNamespace(),
				p.GetName(),
				pvc.GetNamespace(),
				pvc.GetName())
		}
	}

	return nil
}
