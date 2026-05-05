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
package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	powerv1alpha1 "github.com/cluster-power-manager/cluster-power-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envTestClient is a shared client connected to the suite-level envtest API server.
// Initialized in TestMain, available to all envtest tests.
var envTestClient client.Client

// TestMain starts a single envtest API server for the entire envtest suite,
// runs all tests, then tears it down. This avoids the cost of starting/stopping
// envtest per test.
func TestMain(m *testing.M) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	err = powerv1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to add scheme: %v\n", err)
		testEnv.Stop()
		os.Exit(1)
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		testEnv.Stop()
		os.Exit(1)
	}

	// Create the test namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PowerNamespace}}
	_ = cl.Create(context.TODO(), ns)

	envTestClient = cl

	code := m.Run()

	testEnv.Stop()
	os.Exit(code)
}

// setupEnvTest starts a standalone envtest API server and returns a client and
// cleanup function. Use this only when a test needs a fully isolated environment.
// Most tests should use envTestClient instead.
func setupEnvTest(t *testing.T) (client.Client, func()) {
	t.Helper()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
		},
	}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to start envtest")
	require.NotNil(t, cfg)

	err = powerv1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	// Create the test namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PowerNamespace}}
	_ = cl.Create(context.TODO(), ns)

	return cl, func() {
		err := testEnv.Stop()
		if err != nil {
			t.Logf("failed to stop envtest: %v", err)
		}
	}
}

// createTestPowerNodeState creates a PowerNodeState for testing.
func createTestPowerNodeState(t *testing.T, cl client.Client, name string) {
	t.Helper()
	pns := &powerv1alpha1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
	}
	require.NoError(t, cl.Create(context.TODO(), pns))
}

// applyNodeInfo simulates what the PowerConfig controller does in production:
// writes NodeInfo to PowerNodeState status via SSA. This anchors the status
// so it's never null when all other controllers release their fields.
func applyNodeInfo(t *testing.T, cl client.Client, pnsName string) {
	t.Helper()
	patchNodeState := &powerv1alpha1.PowerNodeState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "power.cluster-power-manager.github.io/v1alpha1",
			Kind:       "PowerNodeState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pnsName,
			Namespace: PowerNamespace,
		},
		Status: powerv1alpha1.PowerNodeStateStatus{
			NodeInfo: &powerv1alpha1.NodeInfo{
				CPUCapacity:  96,
				Architecture: "amd64",
			},
		},
	}
	err := cl.Status().Patch(context.TODO(), patchNodeState, client.Apply,
		client.FieldOwner(FieldOwnerPowerConfigController), client.ForceOwnership)
	require.NoError(t, err)
}

// deleteTestPowerNodeState deletes a PowerNodeState created during a test.
func deleteTestPowerNodeState(t *testing.T, cl client.Client, name string) {
	t.Helper()
	pns := &powerv1alpha1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
	}
	err := cl.Delete(context.TODO(), pns)
	if err != nil {
		t.Logf("cleanup: failed to delete PowerNodeState %s: %v", name, err)
	}
}

// newTestPowerProfile creates a PowerProfile for testing.
func newTestPowerProfile(name string, shared bool) *powerv1alpha1.PowerProfile {
	return &powerv1alpha1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PowerNamespace},
		Spec:       powerv1alpha1.PowerProfileSpec{Shared: shared},
	}
}

// createTestNode creates a Node with the given labels.
func createTestNode(t *testing.T, cl client.Client, name string, labels map[string]string) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	require.NoError(t, cl.Create(context.TODO(), node))
}

// createTestPowerNodeConfig creates a PowerNodeConfig with the given spec.
func createTestPowerNodeConfig(t *testing.T, cl client.Client, name string, sharedProfile string, matchLabels map[string]string, reserved []powerv1alpha1.ReservedSpec) {
	t.Helper()
	config := &powerv1alpha1.PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
		Spec: powerv1alpha1.PowerNodeConfigSpec{
			SharedPowerProfile: sharedProfile,
			ReservedCPUs:       reserved,
		},
	}
	if matchLabels != nil {
		config.Spec.NodeSelector = powerv1alpha1.NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		}
	}
	require.NoError(t, cl.Create(context.TODO(), config))
}
