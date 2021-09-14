package main

import (
	"fmt"
	"log"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/pkg/errors"
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

type k8sStorageManager struct {
	namespace  string
	kubeclient kubernetes.Interface
}

func (sm *k8sStorageManager) ensurePVC(
	wf *workflow.Workflow,
	org string,
	repo string,
	branch string,
	defaults CacheSpec) error {
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

	name := labelSafe("ci", scope, org, repo)
	if scope == scopeBranch {
		name = labelSafe("ci", scope, org, repo, branch)
	}

	if wfVolName, ok := wf.Annotations[annCacheVolumeName]; ok {
		name = wfVolName
	}

	ls := labels.Set(
		map[string]string{
			labelManagedBy: "kube-ci",
			labelOrg:       labelSafe(org),
			labelRepo:      labelSafe(repo),
			labelScope:     labelSafe(scope),
		})

	if scope == scopeBranch {
		ls[labelBranch] = labelSafe(branch)
	}

	opt := metav1.GetOptions{}

	pv, err := sm.kubeclient.CoreV1().PersistentVolumeClaims(wf.Namespace).Get(name, opt)
	if err == nil {
		for k, v := range ls {
			v2, ok := pv.Labels[k]
			if !ok || v != v2 {
				return errors.New("cache pvc label mismatch")
			}
		}

		if branch != "" {
			if b, ok := pv.Annotations[annBranch]; ok && b != branch {
				return errors.New("cache pvc branch annotation mismatch")
			}
		}

		parms := wf.Spec.Arguments.Parameters
		wf.Spec.Arguments.Parameters = append(parms, workflow.Parameter{
			Name:  paramCacheVolumeClaimName,
			Value: workflow.Int64OrStringPtr(name),
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
	if branch != "" {
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

	_, err = sm.kubeclient.CoreV1().PersistentVolumeClaims(wf.Namespace).Create(pv)
	if err != nil {
		return err
	}

	parms := wf.Spec.Arguments.Parameters
	wf.Spec.Arguments.Parameters = append(parms, workflow.Parameter{
		Name:  paramCacheVolumeClaimName,
		Value: workflow.Int64OrStringPtr(name),
	})

	return nil
}

// when a branch or repo gets deleted (or archived), we delete any pvc
// associated with it. If branch is not "", only that branch is deleted.
func (sm *k8sStorageManager) deletePVC(
	org string,
	repo string,
	branch string,
	action string) error {

	log.Printf("clearing PVCs for %q/%q %q (action: %q)", org, repo, branch, action)
	ls := labels.Set(
		map[string]string{
			labelManagedBy: "kube-ci",
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

	log.Printf("selector: %#v", sel)

	opt := metav1.ListOptions{
		LabelSelector: sel,
	}

	pvcs, err := sm.kubeclient.CoreV1().PersistentVolumeClaims(sm.namespace).List(opt)
	if k8errors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	for _, pvc := range pvcs.Items {
		if branch != "" {
			if b, ok := pvc.Labels[labelBranch]; ok && b != labelSafe(branch) {
				continue
			}
			if b, ok := pvc.Annotations[annBranch]; ok && b != branch {
				continue
			}
		}
		err := sm.kubeclient.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Delete(pvc.GetName(), nil)
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
	}

	return nil
}
