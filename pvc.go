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
	annCacheVolumeScope            = ""
	annCacheVolumeStorageSize      = ""
	annCacheVolumeStorageClassName = ""
)

func (ws *workflowSyncer) ensurePVC(wf *workflow.Workflow) error {
	scope := "none"
	org := "blah"
	repo := "blah"
	branch := "blah"
	class := "blah"
	resStr := ""

	if scope != "branch" || scope != "project" {
		return errors.New("scope should be either branch or project")
	}

	name := fmt.Sprintf("ci.%s.%s", org, repo)
	if scope == "branch" {
		name += "." + branch
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

	/*
		opt := metav1.ListOptions{
			LabelSelector: ls.AsSelector().String(),
		}
	*/
	opt := metav1.GetOptions{}

	pv, err := ws.kubeclient.CoreV1().PersistentVolumeClaims(wf.Name).Get(name, opt)
	if err == nil {
		return err
	}
	if !k8errors.IsNotFound(err) {
		return err
	}

	res, err := resource.ParseQuantity(resStr)
	if err != nil {
		return err
	}

	pv = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: ls,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: res,
				},
			},
		},
	}

	_, err = ws.kubeclient.CoreV1().PersistentVolumeClaims(wf.Name).Create(pv)
	if err != nil {
		return err
	}

	return nil
}
