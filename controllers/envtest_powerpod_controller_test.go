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
	"testing"

	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podresourcesclient"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// createPodReconcilerWithEnvTest creates a PowerPodReconciler using a real envtest API server client.
// PowerLibrary and PodResourcesClient are mocked since they depend on node-local resources.
func createPodReconcilerWithEnvTest(
	t *testing.T,
	cl client.Client,
	podResources []*podresourcesapi.PodResources,
	sharedPoolCPUs []uint,
) *PowerPodReconciler {
	t.Helper()

	s := scheme.Scheme
	_ = powerv1alpha1.AddToScheme(s)

	state, err := podstate.NewState()
	require.NoError(t, err)

	// Mock the PowerLibrary to return the expected pools.
	mockHost := new(hostMock)
	mockHost.On("GetExclusivePool", "performance").Return(createMockPoolWithCPUs([]uint{}))
	mockHost.On("GetExclusivePool", "balance-performance").Return(createMockPoolWithCPUs([]uint{}))
	mockHost.On("GetSharedPool").Return(createMockPoolWithCPUs(sharedPoolCPUs))

	// Mock the PodResourcesClient to return the expected pod resources.
	fakeListResponse := &podresourcesapi.ListPodResourcesResponse{
		PodResources: podResources,
	}
	fakePodResClient := &fakePodResourcesClient{listResponse: fakeListResponse}

	// Create the PodResourcesClient.
	podResourcesClient := &podresourcesclient.PodResourcesClient{
		Client:                fakePodResClient,
		CpuControlPlaneClient: fakePodResClient,
	}

	return &PowerPodReconciler{
		Client:             cl,
		Log:                ctrl.Log.WithName("testing"),
		Scheme:             s,
		State:              state,
		PodResourcesClient: *podResourcesClient,
		PowerLibrary:       mockHost,
	}
}

// createPodReconcilePrereqs creates the common prerequisite resources for pod reconciliation
// envtests: Node, PowerNodeState, and the specified PowerProfiles.
func createPodReconcilePrereqs(t *testing.T, cl client.Client, ctx context.Context, nodeName string, profileNames ...string) {
	t.Helper()

	require.NoError(t, cl.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(16, resource.DecimalSI),
			},
		},
	}))
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	for _, name := range profileNames {
		require.NoError(t, cl.Create(ctx, &powerv1alpha1.PowerProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PowerNamespace},
			Spec:       powerv1alpha1.PowerProfileSpec{},
		}))
	}
}

func TestSSA_PodReconcile_SinglePod(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, "performance")

	// Create a guaranteed pod requesting the performance PowerProfile.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod", Namespace: PowerNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "perf-container",
				Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "perf-container", ContainerID: "cri-o://container-abc",
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	// Update pod status (envtest doesn't set status on create).
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "perf-container", ContainerID: "cri-o://container-abc",
		}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	r := createPodReconcilerWithEnvTest(t, cl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{4, 5},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7},
	)

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	})
	assert.NoError(t, err)

	// Verify PowerNodeState has the pod's exclusive entry via real SSA.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	assert.Equal(t, string(pod.UID), pns.Status.CPUPools.Exclusive[0].PodUID)
	assert.Equal(t, "test-pod", pns.Status.CPUPools.Exclusive[0].Pod)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1)
	pc := pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.Equal(t, "perf-container", pc.Name)
	assert.Equal(t, "performance", pc.PowerProfile)
	assert.ElementsMatch(t, []uint{4, 5}, pc.CPUIDs)

	// Verify SSA field manager ownership for the exclusive entry.
	expectedManager := fmt.Sprintf("powerpod-controller.%s", pod.UID)
	var found bool
	for _, mf := range pns.ManagedFields {
		if mf.Manager == expectedManager {
			found = true
			break
		}
	}
	assert.True(t, found, "expected field manager %q in ManagedFields", expectedManager)
}

func TestSSA_PodReconcile_TwoPodsMerge(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, "performance", "balance-performance")

	// --- Create pod-1 with two containers requesting different profiles. ---
	pod1Obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "c1", Image: "perf-pod",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
						},
					},
				},
				{
					Name: "c2", Image: "perf-pod",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName("power.cluster-power-manager.github.io/balance-performance"): *resource.NewQuantity(2, resource.DecimalSI),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName("power.cluster-power-manager.github.io/balance-performance"): *resource.NewQuantity(2, resource.DecimalSI),
						},
					},
				},
			},
		},
	}
	require.NoError(t, cl.Create(ctx, pod1Obj))
	pod1Obj.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "c1", ContainerID: "cri-o://pod-1-c1"},
			{Name: "c2", ContainerID: "cri-o://pod-1-c2"},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod1Obj))
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "pod-1", Namespace: PowerNamespace}, pod1Obj))

	// --- Create pod-2 with a single container. ---
	pod2Obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "c3", Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod2Obj))
	pod2Obj.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "c3", ContainerID: "cri-o://pod-2-c3"},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod2Obj))
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "pod-2", Namespace: PowerNamespace}, pod2Obj))

	// Get the UIDs of the pods.
	uid1 := string(pod1Obj.UID)
	uid2 := string(pod2Obj.UID)

	// Create a single reconciler that knows about both pods' resources.
	r := createPodReconcilerWithEnvTest(t, cl,
		[]*podresourcesapi.PodResources{
			{
				Name: "pod-1", Namespace: PowerNamespace,
				Containers: []*podresourcesapi.ContainerResources{
					{Name: "c1", CpuIds: []int64{1, 2}},
					{Name: "c2", CpuIds: []int64{5, 6}},
				},
			},
			{
				Name: "pod-2", Namespace: PowerNamespace,
				Containers: []*podresourcesapi.ContainerResources{
					{Name: "c3", CpuIds: []int64{3, 4}},
				},
			},
		},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	)

	// Reconcile both pods with the same reconciler.
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "pod-1", Namespace: PowerNamespace},
	})
	require.NoError(t, err)
	_, err = r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "pod-2", Namespace: PowerNamespace},
	})
	require.NoError(t, err)

	// Verify both pods coexist in PowerNodeState via real SSA merge.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 2, "both pods should be present via correct SSA merge")

	// Collect entries by podUID for stable assertions.
	podEntries := map[string]powerv1alpha1.ExclusiveCPUPoolStatus{}
	for _, entry := range pns.Status.CPUPools.Exclusive {
		podEntries[entry.PodUID] = entry
	}

	// Check pod 1: two containers with different profiles.
	pod1 := podEntries[uid1]
	assert.Equal(t, "pod-1", pod1.Pod)
	require.Len(t, pod1.PowerContainers, 2)
	containersByName := map[string]powerv1alpha1.PowerContainer{}
	for _, pc := range pod1.PowerContainers {
		containersByName[pc.Name] = pc
	}
	assert.Equal(t, "performance", containersByName["c1"].PowerProfile)
	assert.ElementsMatch(t, []uint{1, 2}, containersByName["c1"].CPUIDs)
	assert.Equal(t, "balance-performance", containersByName["c2"].PowerProfile)
	assert.ElementsMatch(t, []uint{5, 6}, containersByName["c2"].CPUIDs)

	// Check pod 2: single container.
	pod2 := podEntries[uid2]
	assert.Equal(t, "pod-2", pod2.Pod)
	require.Len(t, pod2.PowerContainers, 1)
	assert.Equal(t, "performance", pod2.PowerContainers[0].PowerProfile)
	assert.ElementsMatch(t, []uint{3, 4}, pod2.PowerContainers[0].CPUIDs)

	// Verify SSA field manager ownership for both pods.
	managers := map[string]bool{}
	for _, mf := range pns.ManagedFields {
		managers[mf.Manager] = true
	}
	assert.True(t, managers[fmt.Sprintf("powerpod-controller.%s", uid1)], "expected field manager for pod-1")
	assert.True(t, managers[fmt.Sprintf("powerpod-controller.%s", uid2)], "expected field manager for pod-2")
}

// TestSSA_PodReconcile_NoDuplicateContainers verifies that reconciling the same pod
// multiple times does not create duplicate container entries in PowerNodeState.
func TestSSA_PodReconcile_NoDuplicateContainers(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, "performance")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod", Namespace: PowerNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "perf-container",
				Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName("power.cluster-power-manager.github.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
					},
				},
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "perf-container", ContainerID: "cri-o://container-abc",
		}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	r := createPodReconcilerWithEnvTest(t, cl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{1, 5, 8},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	)

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	}

	// Reconcile the same pod twice.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	// Verify PowerNodeState has exactly one entry with one container — no duplicates.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1, "should have exactly one pod entry")
	assert.Equal(t, string(pod.UID), pns.Status.CPUPools.Exclusive[0].PodUID)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1, "should have exactly one container — no duplicates")
	pc := pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.Equal(t, "perf-container", pc.Name)
	assert.Equal(t, "performance", pc.PowerProfile)
	assert.ElementsMatch(t, []uint{1, 5, 8}, pc.CPUIDs)
}

func TestSSA_ExclusivePoolFieldManagerOwnership(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)

	// Create a minimal reconciler with the envtest client.
	r := &PowerPodReconciler{Client: cl}
	logger := ctrl.Log.WithName("testing")

	// Add pod 1.
	err := r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", "pod-1",
		[]powerv1alpha1.PowerContainer{
			{Name: "c1", ID: "cid-1", PowerProfile: "performance", CPUIDs: []uint{1, 2}},
		}, &logger)
	require.NoError(t, err)

	// Add pod 2.
	err = r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-2", "pod-2",
		[]powerv1alpha1.PowerContainer{
			{Name: "c2", ID: "cid-2", PowerProfile: "balance-power", CPUIDs: []uint{3, 4}},
		}, &logger)
	require.NoError(t, err)

	// Both pods should coexist.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	require.NotNil(t, pns.Status.CPUPools)
	assert.Len(t, pns.Status.CPUPools.Exclusive, 2, "both pod entries should be present")
}

func TestSSA_ExclusivePoolPodRemovalPreservesOtherPods(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)

	r := &PowerPodReconciler{Client: cl}
	logger := ctrl.Log.WithName("testing")

	// Add two pods.
	err := r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", "pod-1",
		[]powerv1alpha1.PowerContainer{
			{Name: "c1", ID: "cid-1", PowerProfile: "performance", CPUIDs: []uint{1, 2}},
		}, &logger)
	require.NoError(t, err)

	err = r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-2", "pod-2",
		[]powerv1alpha1.PowerContainer{
			{Name: "c2", ID: "cid-2", PowerProfile: "performance", CPUIDs: []uint{3, 4}},
		}, &logger)
	require.NoError(t, err)

	// Remove pod 1.
	err = r.removePowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", &logger)
	require.NoError(t, err)

	// Only pod 2 should remain.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	require.NotNil(t, pns.Status.CPUPools)
	assert.Len(t, pns.Status.CPUPools.Exclusive, 1, "only pod-2 should remain")
	assert.Equal(t, "uid-2", pns.Status.CPUPools.Exclusive[0].PodUID)
}
