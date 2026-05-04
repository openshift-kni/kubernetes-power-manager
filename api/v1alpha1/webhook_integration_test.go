//go:build envtest

/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var (
	envtestClient client.Client
	envtestCancel context.CancelFunc
	envtestEnv    *envtest.Environment
)

func TestMain(m *testing.M) {
	envtestEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "config", "webhook"),
			},
		},
	}

	cfg, err := envtestEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	exit := func(code int) {
		if envtestCancel != nil {
			envtestCancel()
		}
		if err := envtestEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
		}
		os.Exit(code)
	}

	if err := AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add scheme: %v\n", err)
		exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	envtestCancel = cancel

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    envtestEnv.WebhookInstallOptions.LocalServingHost,
			Port:    envtestEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: envtestEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		exit(1)
	}

	if err := SetupPowerNodeConfigWebhookWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup PowerNodeConfig webhook: %v\n", err)
		exit(1)
	}
	if err := SetupUncoreWebhookWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup Uncore webhook: %v\n", err)
		exit(1)
	}
	if err := SetupPowerProfileWebhookWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup PowerProfile webhook: %v\n", err)
		exit(1)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	envtestClient = mgr.GetClient()

	// Create test namespace before probing webhooks.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultKPMNamespace}}
	if err := envtestClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		exit(1)
	}

	// Wait for webhook server to be ready by attempting an intentionally invalid
	// create. A webhook rejection (IsInvalid) confirms the server is listening.
	ready := false
	for i := 0; i < 100; i++ {
		probe := &Uncore{
			ObjectMeta: metav1.ObjectMeta{Name: "webhook-readiness-probe", Namespace: defaultKPMNamespace},
			Spec:       UncoreSpec{SysMin: uintPtr(2), SysMax: uintPtr(1)},
		}
		if err := envtestClient.Create(ctx, probe); apierrors.IsInvalid(err) {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintln(os.Stderr, "webhook server not ready after 10s")
		exit(1)
	}

	// Run the tests.
	code := m.Run()

	// Shutdown the manager and envtest.
	cancel()
	if err := envtestEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func TestEnvTestWebhook_PowerNodeConfig(t *testing.T) {
	ctx := context.TODO()

	// --- Setup: create profiles and a node used across subtests ---
	sharedProf := &PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-prof", Namespace: defaultKPMNamespace},
		Spec:       PowerProfileSpec{Shared: true},
	}
	perfProf := &PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "perf-prof", Namespace: defaultKPMNamespace},
		Spec:       PowerProfileSpec{Shared: false},
	}
	resvProf := &PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "res-prof", Namespace: defaultKPMNamespace},
		Spec:       PowerProfileSpec{},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pnc-worker-1", Labels: map[string]string{"zone": "us-east"}},
	}

	require.NoError(t, envtestClient.Create(ctx, sharedProf))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, sharedProf)) })
	require.NoError(t, envtestClient.Create(ctx, perfProf))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, perfProf)) })
	require.NoError(t, envtestClient.Create(ctx, resvProf))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, resvProf)) })
	require.NoError(t, envtestClient.Create(ctx, node))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, node)) })

	// --- Rejected creates (no resources persist) ---

	t.Run("create/reject missing profile", func(t *testing.T) {
		config := &PowerNodeConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "pnc-bad-missing", Namespace: defaultKPMNamespace},
			Spec:       PowerNodeConfigSpec{SharedPowerProfile: "nonexistent"},
		}
		err := envtestClient.Create(ctx, config)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "spec.sharedPowerProfile: Not found: \"nonexistent\"")
	})

	t.Run("create/reject not-shared profile", func(t *testing.T) {
		config := &PowerNodeConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "pnc-bad-notshared", Namespace: defaultKPMNamespace},
			Spec:       PowerNodeConfigSpec{SharedPowerProfile: "perf-prof"},
		}
		err := envtestClient.Create(ctx, config)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "spec.shared set to true")
	})

	t.Run("create/reject overlapping CPUs", func(t *testing.T) {
		config := &PowerNodeConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "pnc-bad-overlap", Namespace: defaultKPMNamespace},
			Spec: PowerNodeConfigSpec{
				SharedPowerProfile: "shared-prof",
				ReservedCPUs: []ReservedSpec{
					{Cores: []uint{0, 1, 2}, PowerProfile: "res-prof"},
					{Cores: []uint{2, 3, 4}, PowerProfile: "res-prof"},
				},
			},
		}
		err := envtestClient.Create(ctx, config)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "CPU 2 already appears in reservedCPUs[0]")
	})

	// --- Create a valid PowerNodeConfig ---

	config := &PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "pnc-valid", Namespace: defaultKPMNamespace},
		Spec: PowerNodeConfigSpec{
			SharedPowerProfile: "shared-prof",
			NodeSelector: NodeSelector{
				LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "us-east"}},
			},
			ReservedCPUs: []ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "res-prof"},
			},
		},
	}
	require.NoError(t, envtestClient.Create(ctx, config))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, config)) })

	// --- Rejected creates that depend on the valid resource existing ---

	t.Run("create/reject nodeSelector conflict", func(t *testing.T) {
		conflicting := &PowerNodeConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "pnc-bad-conflict", Namespace: defaultKPMNamespace},
			Spec: PowerNodeConfigSpec{
				SharedPowerProfile: "shared-prof",
				NodeSelector: NodeSelector{
					LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "us-east"}},
				},
			},
		}
		err := envtestClient.Create(ctx, conflicting)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), `conflicts with "pnc-valid": identical nodeSelector`)
	})

	// --- Rejected updates ---

	t.Run("update/reject overlapping CPUs and missing profiles", func(t *testing.T) {
		require.NoError(t, envtestClient.Get(ctx, client.ObjectKeyFromObject(config), config))
		config.Spec.SharedPowerProfile = "nonexistent"
		config.Spec.ReservedCPUs = []ReservedSpec{
			{Cores: []uint{0, 1, 2}, PowerProfile: "res-prof"},
			{Cores: []uint{2, 3, 4}, PowerProfile: "also-missing"},
		}
		err := envtestClient.Update(ctx, config)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "CPU 2 already appears in reservedCPUs[0]")
		assert.Contains(t, err.Error(), "spec.sharedPowerProfile: Not found: \"nonexistent\"")
		assert.Contains(t, err.Error(), "spec.reservedCPUs[1].powerProfile: Not found: \"also-missing\"")
	})

	// --- Valid update ---

	t.Run("update/valid", func(t *testing.T) {
		require.NoError(t, envtestClient.Get(ctx, client.ObjectKeyFromObject(config), config))
		config.Spec.ReservedCPUs = []ReservedSpec{
			{Cores: []uint{2, 3}, PowerProfile: "res-prof"},
		}
		config.Spec.SharedPowerProfile = "shared-prof"
		require.NoError(t, envtestClient.Update(ctx, config))
	})

}

func TestEnvTestWebhook_Uncore(t *testing.T) {
	ctx := context.TODO()

	// --- Setup: create a node for conflict detection ---
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "uc-worker-1", Labels: map[string]string{"zone": "us-east"}},
	}
	require.NoError(t, envtestClient.Create(ctx, node))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, node)) })

	// --- Rejected creates (no resources persist) ---

	t.Run("create/reject sysMin > sysMax", func(t *testing.T) {
		uncore := &Uncore{
			ObjectMeta: metav1.ObjectMeta{Name: "uc-bad-minmax", Namespace: defaultKPMNamespace},
			Spec:       UncoreSpec{SysMin: uintPtr(2400), SysMax: uintPtr(800)},
		}
		err := envtestClient.Create(ctx, uncore)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "sysMin (2400) must be <= sysMax (800)")
	})

	t.Run("create/reject duplicate dieSelector", func(t *testing.T) {
		uncore := &Uncore{
			ObjectMeta: metav1.ObjectMeta{Name: "uc-bad-dupdie", Namespace: defaultKPMNamespace},
			Spec: UncoreSpec{
				DieSelectors: &[]DieSelector{
					{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
					{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
				},
			},
		}
		err := envtestClient.Create(ctx, uncore)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "duplicate: package 0, die 0 already specified in dieSelector[0]")
	})

	// --- Create a valid Uncore ---

	uncore := &Uncore{
		ObjectMeta: metav1.ObjectMeta{Name: "uc-valid", Namespace: defaultKPMNamespace},
		Spec: UncoreSpec{
			SysMin: uintPtr(800),
			SysMax: uintPtr(2400),
			DieSelectors: &[]DieSelector{
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(1), Die: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
			},
			NodeSelector: NodeSelector{
				LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "us-east"}},
			},
		},
	}
	require.NoError(t, envtestClient.Create(ctx, uncore))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, uncore)) })

	// --- Rejected creates that depend on the valid resource existing ---

	t.Run("create/reject nodeSelector conflict", func(t *testing.T) {
		conflicting := &Uncore{
			ObjectMeta: metav1.ObjectMeta{Name: "uc-bad-conflict", Namespace: defaultKPMNamespace},
			Spec: UncoreSpec{
				SysMin: uintPtr(800),
				SysMax: uintPtr(2400),
				NodeSelector: NodeSelector{
					LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"zone": "us-east"}},
				},
			},
		}
		err := envtestClient.Create(ctx, conflicting)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), `conflicts with "uc-valid": identical nodeSelector`)
	})

	// --- Rejected updates ---

	t.Run("update/reject sysMin > sysMax, die min > max, and duplicate dieSelector", func(t *testing.T) {
		require.NoError(t, envtestClient.Get(ctx, client.ObjectKeyFromObject(uncore), uncore))
		uncore.Spec.SysMin = uintPtr(2400)
		uncore.Spec.SysMax = uintPtr(800)
		uncore.Spec.DieSelectors = &[]DieSelector{
			{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(2400), Max: uintPtr(800)},
			{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
		}
		err := envtestClient.Update(ctx, uncore)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err))
		assert.Contains(t, err.Error(), "sysMin (2400) must be <= sysMax (800)")
		assert.Contains(t, err.Error(), "min (2400) must be <= max (800)")
		assert.Contains(t, err.Error(), "duplicate: package 0, die 0 already specified in dieSelector[0]")
	})

	// --- Valid update ---

	t.Run("update/valid", func(t *testing.T) {
		require.NoError(t, envtestClient.Get(ctx, client.ObjectKeyFromObject(uncore), uncore))
		uncore.Spec.SysMin = uintPtr(1000)
		uncore.Spec.SysMax = uintPtr(2000)
		uncore.Spec.DieSelectors = nil
		require.NoError(t, envtestClient.Update(ctx, uncore))
	})
}

func TestEnvTestWebhook_PowerProfile(t *testing.T) {
	ctx := context.TODO()

	// --- Setup: create a profile and a PowerNodeConfig that references it ---
	profile := &PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "pp-shared", Namespace: defaultKPMNamespace},
		Spec:       PowerProfileSpec{Shared: true},
	}
	require.NoError(t, envtestClient.Create(ctx, profile))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, profile)) })

	pnc := &PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "pp-test-config", Namespace: defaultKPMNamespace},
		Spec: PowerNodeConfigSpec{
			SharedPowerProfile: "pp-shared",
		},
	}
	require.NoError(t, envtestClient.Create(ctx, pnc))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, pnc)) })

	// --- Rejected deletes ---

	t.Run("delete/reject referenced by PowerNodeConfig", func(t *testing.T) {
		err := envtestClient.Delete(ctx, profile)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err))
		assert.Contains(t, err.Error(), "cannot delete PowerProfile")
		assert.Contains(t, err.Error(), "referenced by PowerNodeConfig(s): pp-test-config (sharedPowerProfile)")
	})

	// Create two pods that reference the profile, then verify both PNC and pod reasons appear.
	podA := testPod("pp-test-pod-a", defaultKPMNamespace, corev1.ResourceList{
		corev1.ResourceName(extendedResourcePrefix + "pp-shared"): resource.MustParse("1"),
	})
	podB := testPod("pp-test-pod-b", defaultKPMNamespace, corev1.ResourceList{
		corev1.ResourceName(extendedResourcePrefix + "pp-shared"): resource.MustParse("2"),
	})
	require.NoError(t, envtestClient.Create(ctx, podA))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, podA)) })
	require.NoError(t, envtestClient.Create(ctx, podB))
	t.Cleanup(func() { require.NoError(t, envtestClient.Delete(ctx, podB)) })

	t.Run("delete/reject referenced by both PowerNodeConfig and Pods", func(t *testing.T) {
		err := envtestClient.Delete(ctx, profile)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err))
		assert.Contains(t, err.Error(), "referenced by PowerNodeConfig(s): pp-test-config (sharedPowerProfile)")
		assert.Contains(t, err.Error(), "referenced by Pod(s):")
		assert.Contains(t, err.Error(), "power-manager/pp-test-pod-a")
		assert.Contains(t, err.Error(), "power-manager/pp-test-pod-b")
	})

	// --- Rejected updates ---

	t.Run("update/reject shared true to false while referenced", func(t *testing.T) {
		require.NoError(t, envtestClient.Get(ctx, client.ObjectKeyFromObject(profile), profile))
		profile.Spec.Shared = false
		err := envtestClient.Update(ctx, profile)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err))
		assert.Contains(t, err.Error(), "cannot change spec.shared from true to false")
		assert.Contains(t, err.Error(), "pp-test-config")
	})

	// --- Valid delete: unreferenced profile ---

	t.Run("delete/valid unreferenced profile", func(t *testing.T) {
		unusedProfile := &PowerProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "pp-unused", Namespace: defaultKPMNamespace},
			Spec:       PowerProfileSpec{},
		}
		require.NoError(t, envtestClient.Create(ctx, unusedProfile))
		require.NoError(t, envtestClient.Delete(ctx, unusedProfile))
	})
}
