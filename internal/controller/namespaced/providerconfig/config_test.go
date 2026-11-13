// SPDX-FileCopyrightText: 2026 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"context"
	"testing"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	runtimepc "github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/providerconfig"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane-contrib/provider-upjet-zitadel/apis/namespaced/v1beta1"
)

type testManager struct {
	manager.Manager
	kube   client.Client
	scheme *runtime.Scheme
}

func (m testManager) GetClient() client.Client   { return m.kube }
func (m testManager) GetScheme() *runtime.Scheme { return m.scheme }

// Both previews use the same chart names. Deleting one must wait for its own
// resources, then finish even while the other preview and cluster users remain.
func TestPreviewProviderConfigLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := v1beta1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const name = "harmony-gateway-product-zitadel"
	isController := true
	configs := []client.Object{
		&v1beta1.ProviderConfig{TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String(), Kind: "ProviderConfig"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "preview-a"}},
		&v1beta1.ProviderConfig{TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String(), Kind: "ProviderConfig"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "preview-b"}},
		&v1beta1.ClusterProviderConfig{TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String(), Kind: "ClusterProviderConfig"}, ObjectMeta: metav1.ObjectMeta{Name: name}},
	}
	usage := func(ns, id, kind string) *v1beta1.ProviderConfigUsage {
		u := &v1beta1.ProviderConfigUsage{ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: ns,
			Labels:          map[string]string{xpv1.LabelKeyProviderName: name, xpv1.LabelKeyProviderKind: kind},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "app.zitadel.m.crossplane.io/v1alpha1", Kind: "OIDC", Name: id, UID: types.UID(id), Controller: &isController}},
		}}
		u.SetProviderConfigReference(xpv1.ProviderConfigReference{Name: name, Kind: kind})
		return u
	}
	local := usage("preview-a", "local", "ProviderConfig")
	foreign := usage("preview-b", "foreign", "ProviderConfig")
	clusterA := usage("preview-a", "cluster-a", "ClusterProviderConfig")
	clusterB := usage("preview-b", "cluster-b", "ClusterProviderConfig")
	objects := make([]client.Object, 0, len(configs)+4)
	objects = append(objects, configs...)
	objects = append(objects, local, foreign, clusterA, clusterB)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ProviderConfig{}, &v1beta1.ClusterProviderConfig{}).
		WithObjects(objects...).Build()
	mgr := testManager{kube: kube, scheme: scheme}
	kinds := resource.ProviderConfigKinds{Config: v1beta1.ProviderConfigGroupVersionKind, Usage: v1beta1.ProviderConfigUsageGroupVersionKind, UsageList: v1beta1.ProviderConfigUsageListGroupVersionKind}
	namespaced := runtimepc.NewReconciler(mgr, kinds)
	kinds.Config = v1beta1.ClusterProviderConfigGroupVersionKind
	cluster := runtimepc.NewReconciler(mgr, kinds)
	run := func(r *runtimepc.Reconciler, ns string) {
		t.Helper()
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		if err != nil || result.RequeueAfter != 0 {
			t.Fatalf("reconcile %q: %+v, %v", ns, result, err)
		}
	}
	run(namespaced, "preview-a")
	run(namespaced, "preview-b")
	run(cluster, "")
	for _, ns := range []string{"preview-a", "preview-b"} {
		pc := &v1beta1.ProviderConfig{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pc); err != nil {
			t.Fatal(err)
		}
		if pc.GetUsers() != 1 {
			t.Fatalf("%s users = %d; want only its one local user", ns, pc.GetUsers())
		}
	}
	clusterPC := &v1beta1.ClusterProviderConfig{}
	if err := kube.Get(ctx, types.NamespacedName{Name: name}, clusterPC); err != nil {
		t.Fatal(err)
	}
	if clusterPC.GetUsers() != 2 {
		t.Fatalf("cluster users = %d; want both namespaces", clusterPC.GetUsers())
	}
	if err := kube.Delete(ctx, configs[0]); err != nil {
		t.Fatal(err)
	}
	run(namespaced, "preview-a")
	deleting := &v1beta1.ProviderConfig{}
	key := types.NamespacedName{Namespace: "preview-a", Name: name}
	if err := kube.Get(ctx, key, deleting); err != nil {
		t.Fatalf("local user must block deletion: %v", err)
	}
	if deleting.GetDeletionTimestamp() == nil || deleting.GetUsers() != 1 {
		t.Fatal("expected deleting config with one local user")
	}
	if err := kube.Delete(ctx, local); err != nil {
		t.Fatal(err)
	}
	run(namespaced, "preview-a")
	if err := kube.Get(ctx, key, deleting); !apierrors.IsNotFound(err) {
		t.Fatalf("unused config must delete despite foreign users: %v", err)
	}
	for _, obj := range []client.Object{configs[1], clusterPC, foreign, clusterA, clusterB} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("other preview/cluster resource changed: %v", err)
		}
	}
}
