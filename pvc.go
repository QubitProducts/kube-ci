package main

import (
	"fmt"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	annCacheVolumeName             = "kube-ci.qutics.com/cacheName"
	annCacheVolumeScope            = "kube-ci.qutics.com/cacheScope"
	annCacheVolumeStorageSize      = "kube-ci.qutics.com/cacheSize"
	annCacheVolumeStorageClassName = "kube-ci.qutics.com/cacheStorageClassName"
)

func (ws *workflowSyncer) ensurePVC(
	wf *workflow.Workflow,
	org string,
	repo string,
	branch string,
	defaults CacheSpec) error {
	scope := defaults.Scope
	if wfScope, ok := wf.Annotations[annCacheVolumeScope]; ok {
		scope = wfScope
	}
	if scope == "none" || scope == "" {
		return nil
	}
	if scope != "branch" && scope != "project" {
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

	name := fmt.Sprintf("ci.%s.%s.%s", scope, org, repo)
	if scope == "branch" {
		name += "." + branch
	}
	if wfVolName, ok := wf.Annotations[annCacheVolumeName]; ok {
		name = wfVolName
	}

	ls := labels.Set(
		map[string]string{
			"managedBy": "kube-ci",
			"org":       org,
			"repo":      repo,
		})

	if scope == "branch" {
		ls["branch"] = branch
	}

	opt := metav1.GetOptions{}

	pv, err := ws.kubeclient.CoreV1().PersistentVolumeClaims(wf.Namespace).Get(name, opt)
	if err == nil {
		for k, v := range ls {
			v2, ok := pv.Labels[k]
			if !ok || v != v2 {
				return errors.New("cache pvc label mismatch")
			}
		}

		return err
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

	pv = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: wf.Namespace,
			Labels:    ls,
		},
		Spec: spec,
	}

	_, err = ws.kubeclient.CoreV1().PersistentVolumeClaims(wf.Namespace).Create(pv)
	if err != nil {
		return err
	}

	return nil
}