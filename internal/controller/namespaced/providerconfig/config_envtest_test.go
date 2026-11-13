//go:build integration

// SPDX-FileCopyrightText: 2026 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	xpcontroller "github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/upjet/v2/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/crossplane-contrib/provider-upjet-zitadel/apis/namespaced/v1beta1"
)

// Exercise real API finalizers and watch-driven reconciliation without an
// external Zitadel instance or access to the developer's Kubernetes context.
func TestPreviewCleanupWithAPIServer(t *testing.T) {
	env := &envtest.Environment{ErrorIfCRDPathMissing: true}
	for _, name := range []string{"providerconfigs", "clusterproviderconfigs", "providerconfigusages"} {
		env.CRDDirectoryPaths = append(env.CRDDirectoryPaths, filepath.Join("..", "..", "..", "..", "package", "crds", "zitadel.m.crossplane.io_"+name+".yaml"))
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1beta1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Setup(mgr, controller.Options{Options: xpcontroller.Options{
		Logger: logging.NewNopLogger(), GlobalRateLimiter: ratelimiter.NewGlobal(10), MaxConcurrentReconciles: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("controller manager did not stop")
		}
	})
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const name = "harmony-gateway-product-zitadel"
	isController := true
	for _, ns := range []string{"preview-a", "preview-b"} {
		if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		pc := &v1beta1.ProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1beta1.ProviderConfigSpec{Credentials: v1beta1.ProviderCredentials{Source: xpv1.CredentialsSourceNone}}}
		if err := kube.Create(ctx, pc); err != nil {
			t.Fatal(err)
		}
		usage := &v1beta1.ProviderConfigUsage{ObjectMeta: metav1.ObjectMeta{Name: "oidc", Namespace: ns,
			Labels:          map[string]string{xpv1.LabelKeyProviderName: name, xpv1.LabelKeyProviderKind: "ProviderConfig"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "app.zitadel.m.crossplane.io/v1alpha1", Kind: "OIDC", Name: "oidc", UID: types.UID(ns), Controller: &isController}},
		}}
		usage.SetProviderConfigReference(xpv1.ProviderConfigReference{Name: name, Kind: "ProviderConfig"})
		usage.SetResourceReference(xpv1.TypedReference{APIVersion: "app.zitadel.m.crossplane.io/v1alpha1", Kind: "OIDC", Name: "oidc"})
		if err := kube.Create(ctx, usage); err != nil {
			t.Fatal(err)
		}
	}
	eventually := func(description string, fn func(context.Context) (bool, error)) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, fn); err != nil {
			t.Fatalf("%s: %v", description, err)
		}
	}
	for _, ns := range []string{"preview-a", "preview-b"} {
		eventually(ns+" accounts for only its own user", func(ctx context.Context) (bool, error) {
			pc := &v1beta1.ProviderConfig{}
			err := kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pc)
			return err == nil && pc.GetUsers() == 1 && len(pc.GetFinalizers()) > 0, err
		})
	}
	pc := &v1beta1.ProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "preview-a"}}
	if err := kube.Delete(ctx, pc); err != nil {
		t.Fatal(err)
	}
	eventually("deletion waits for the local usage", func(ctx context.Context) (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(pc), pc)
		return err == nil && pc.GetDeletionTimestamp() != nil && pc.GetCondition("Terminating").Reason == "InUse", err
	})
	if err := kube.Delete(ctx, &v1beta1.ProviderConfigUsage{ObjectMeta: metav1.ObjectMeta{Name: "oidc", Namespace: "preview-a"}}); err != nil {
		t.Fatal(err)
	}
	eventually("usage deletion event completes preview-a cleanup", func(ctx context.Context) (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(pc), pc)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	other := &v1beta1.ProviderConfig{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: "preview-b", Name: name}, other); err != nil {
		t.Fatal(err)
	}
	if other.GetUsers() != 1 || other.GetDeletionTimestamp() != nil {
		t.Fatal("other preview was changed by cleanup")
	}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: "preview-b", Name: "oidc"}, &v1beta1.ProviderConfigUsage{}); err != nil {
		t.Fatal(err)
	}
}
