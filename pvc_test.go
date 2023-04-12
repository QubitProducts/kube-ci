package kubeci

import (
	"context"
	"testing"

	"github.com/google/go-github/v45/github"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newPVC(name, namespace, org, repo, scope, branch string) runtime.Object {
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	ls := map[string]string{
		"somelabel": "unrelated", // assume we can have arbitrary labels on pvcs
	}
	anns := map[string]string{}

	if org != "" {
		ls["managedBy"] = "kube-ci"
		ls[labelOrg] = labelSafe(org)
	}
	if repo != "" {
		ls[labelRepo] = labelSafe(repo)
	}
	if scope != "" {
		ls[labelScope] = labelSafe(scope)
	}
	if branch != "" {
		ls[labelBranch] = labelSafe(branch)
		anns[annBranch] = branch
	}

	pvc.SetLabels(ls)
	pvc.SetAnnotations(anns)

	return pvc
}

func TestWebhookDeleteBranchEvent(t *testing.T) {
	ctx := context.Background()

	expectdels := map[string]struct{}{}

	pvcs := []runtime.Object{}
	pvcs = append(pvcs, newPVC("unrelated", "default", "", "", "", ""))
	pvcs = append(pvcs, newPVC("unrelated", "argo", "", "", "", ""))
	pvcs = append(pvcs, newPVC("somepvcwecreated", "argo", "myorg", "myrepo", "project", ""))
	pvcs = append(pvcs, newPVC("somepvcwecreated2", "argo", "myorg", "myrepo", "branch", "mybranch"))
	expectdels["argo/somepvcwecreated2"] = struct{}{}
	pvcs = append(pvcs, newPVC("somepvcwecreated3", "argo", "myorg", "myrepo", "branch", "mybranch2"))
	pvcs = append(pvcs, newPVC("someother", "argo", "myorg", "myrepo2", "project", ""))
	client := k8sfake.NewSimpleClientset(pvcs...)

	sm := &K8sStorageManager{
		KubeClient: client,
		ManagedBy:  "kube-ci",
	}
	hh := &HookHandler{
		Storage: sm,
	}

	ev := &github.DeleteEvent{
		Ref:     github.String("mybranch"),
		RefType: github.String("branch"),
		Repo: &github.Repository{
			Name: github.String("myrepo"),
			Owner: &github.User{
				Login: github.String("myorg"),
			},
		},
	}

	_, status := hh.webhookDeleteBranchEvent(ctx, ev)
	if status != "OK" {
		t.Fatalf("unexpected status, %v", status)
		return
	}

	dels := 0
	for _, act := range client.Actions() {
		switch act := act.(type) {
		case k8stesting.DeleteActionImpl:
			dels++
			key := act.GetNamespace() + "/" + act.Name
			if _, ok := expectdels[key]; !ok {
				t.Fatalf("unexpected delete action (key: %s) %v", key, act)
				return
			}
		}
	}

	if dels != len(expectdels) {
		t.Fatalf("wrong number of deletes, want %v, got %v", len(expectdels), dels)
		return
	}
}

func TestWebhookRepositoryDeleteEvent(t *testing.T) {
	ctx := context.Background()
	pvcs := []runtime.Object{}

	expectdels := map[string]struct{}{}

	pvcs = append(pvcs, newPVC("unrelated", "default", "", "", "", ""))
	pvcs = append(pvcs, newPVC("unrelated", "argo", "", "", "", ""))

	pvcs = append(pvcs, newPVC("somepvcwecreated", "argo", "myorg", "myrepo", "project", ""))
	expectdels["argo/somepvcwecreated"] = struct{}{}
	pvcs = append(pvcs, newPVC("somepvcwecreated2", "argo", "myorg", "myrepo", "branch", "mybranch"))
	expectdels["argo/somepvcwecreated2"] = struct{}{}
	pvcs = append(pvcs, newPVC("somepvcwecreated3", "argo", "myorg", "myrepo", "branch", "mybranch2"))
	expectdels["argo/somepvcwecreated3"] = struct{}{}
	pvcs = append(pvcs, newPVC("someother", "argo", "myorg", "myrepo2", "project", ""))

	client := k8sfake.NewSimpleClientset(pvcs...)

	sm := &K8sStorageManager{
		KubeClient: client,
		ManagedBy:  "kube-ci",
	}
	hh := HookHandler{
		Storage: sm,
	}

	ev := &github.RepositoryEvent{
		Action: github.String("deleted"),
		Repo: &github.Repository{
			Name: github.String("myrepo"),
			Owner: &github.User{
				Login: github.String("myorg"),
			},
		},
	}

	_, status := hh.webhookRepositoryDeleteEvent(ctx, ev)
	if status != "OK" {
		t.Fatalf("unexpected status, %v", status)
		return
	}

	dels := 0
	for _, act := range client.Actions() {
		switch act := act.(type) {
		case k8stesting.DeleteActionImpl:
			dels++
			key := act.GetNamespace() + "/" + act.Name
			if _, ok := expectdels[key]; !ok {
				t.Fatalf("unexpected delete action (key: %s) %v", key, act)
				return
			}
		}
	}

	if dels != len(expectdels) {
		t.Fatalf("wrong number of deletes, want %v, got %v", len(expectdels), dels)
		return
	}
}

func TestWebhookDeleteBranchEvent_BadName(t *testing.T) {
	ctx := context.Background()

	pvcs := []runtime.Object{}
	pvcs = append(pvcs, newPVC("unrelated", "default", "", "", "", ""))
	pvcs = append(pvcs, newPVC("unrelated", "argo", "", "", "", ""))
	pvcs = append(pvcs, newPVC("somepvcwecreated", "argo", "myorg", "myrepo", "project", ""))
	client := k8sfake.NewSimpleClientset(pvcs...)

	sm := &K8sStorageManager{
		KubeClient: client,
		ManagedBy:  "kube-ci",
	}
	hh := &HookHandler{
		Storage: sm,
	}

	ev := &github.DeleteEvent{
		Ref:     github.String("-my..bad..branch...name-"),
		RefType: github.String("branch"),
		Repo: &github.Repository{
			Name: github.String("myrepo"),
			Owner: &github.User{
				Login: github.String("myorg"),
			},
		},
	}

	_, status := hh.webhookDeleteBranchEvent(ctx, ev)
	if status != "OK" {
		t.Fatalf("unexpected status, %v", status)
		return
	}

	for _, act := range client.Actions() {
		switch act := act.(type) {
		case k8stesting.DeleteActionImpl:
			t.Errorf("unexpected delete of %v/%v ", act.Namespace, act.Name)
		}
	}
}
