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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerNodeStateSpec defines the desired state of PowerNodeState
// This is intentionally empty as PowerNodeState is a status-only CRD
type PowerNodeStateSpec struct {
}

// PowerNodeStateStatus defines the observed state of PowerNodeState.
// Each field is owned by a specific controller using Server-Side Apply (SSA).
// IMPORTANT: All status updates to PowerNodeState MUST use SSA (client.Apply).
// Non-SSA updates (Status().Update or MergePatch) will break field ownership
// tracking and cause incorrect pruning behavior.
type PowerNodeStateStatus struct {
	// NodeInfo contains static node information written once by the PowerConfig controller.
	// Acts as an SSA anchor: because this field is always present and owned, the status
	// object is never empty.
	// Owned by: PowerConfig controller
	// +optional
	NodeInfo *NodeInfo `json:"nodeInfo,omitempty"`

	// PowerProfiles contains the status of power profiles on this node
	// Owned by: PowerProfile controller
	// +optional
	// +listType=map
	// +listMapKey=name
	PowerProfiles []PowerNodeProfileStatus `json:"powerProfiles,omitzero"`

	// CPUPools contains the status of CPU pools on this node
	// Owned by: PowerNodeConfig controller (shared, reserved) and PowerPod controller (exclusive)
	// +optional
	CPUPools *CPUPoolsStatus `json:"cpuPools,omitempty"`

	// Uncore contains the status of uncore frequency configuration on this node
	// Owned by: Uncore controller
	// +optional
	Uncore *NodeUncoreStatus `json:"uncore,omitempty"`
}

// NodeInfo contains static information about the node, written once by the PowerConfig controller.
type NodeInfo struct {
	// CPUCapacity is the total number of CPUs on the node (from node.Status.Capacity)
	CPUCapacity int `json:"cpuCapacity"`

	// Architecture is the CPU architecture of the node (e.g., "amd64", "arm64")
	Architecture string `json:"architecture"`
}

// PowerNodeProfileStatus represents the status of a power profile on this node
type PowerNodeProfileStatus struct {
	// Name is the name of the PowerProfile CR
	Name string `json:"name"`

	// Config is the configuration of the PowerProfile
	Config string `json:"config"`

	// Errors contains any errors encountered while applying this profile on this node
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// CPUPoolsStatus contains the status of all CPU pools on this node
type CPUPoolsStatus struct {
	// Shared contains the status of the shared CPU pool
	// Owned by: PowerNodeConfig controller (shared, reserved)
	// +optional
	Shared *SharedCPUPoolStatus `json:"shared,omitempty"`

	// Reserved contains the status of the reserved CPU pools
	// Owned by: PowerNodeConfig controller (shared, reserved)
	// +optional
	Reserved []ReservedCPUPoolStatus `json:"reserved,omitzero"`

	// Exclusive contains the status of exclusive CPU pools
	// Owned by: PowerPod controller (exclusive)
	// +optional
	// +listType=map
	// +listMapKey=podUID
	Exclusive []ExclusiveCPUPoolStatus `json:"exclusive,omitzero"`
}

// SharedCPUPoolStatus represents the status of the shared CPU pool
type SharedCPUPoolStatus struct {
	// PowerProfile is the name of the PowerProfile applied to this pool
	PowerProfile string `json:"powerProfile"`

	// PowerNodeConfig is the name of the PowerNodeConfig applied to this pool
	PowerNodeConfig string `json:"powerNodeConfig"`

	// CPUIDs are the CPU IDs in this pool, pretty-printed as ranges (e.g. "2-23,46,47")
	CPUIDs string `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring this pool
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// ReservedCPUPoolStatus represents the status of a reserved CPU pool
type ReservedCPUPoolStatus struct {
	// PowerProfileCPUs are the PowerProfile and CPUIDs in this pool
	PowerProfileCPUs []PowerProfileCPUs `json:"powerProfileCPUs"`

	// PowerNodeConfig is the name of the PowerNodeConfig applied to this pool
	PowerNodeConfig string `json:"powerNodeConfig"`
}

// PowerProfileCPUs contains information about CPUs in a pool using a specific PowerProfile
type PowerProfileCPUs struct {
	// PowerProfile is the name of the PowerProfile applied to this pool
	PowerProfile string `json:"powerProfile"`

	// CPUIDs are the CPU IDs in this pool, pretty-printed as ranges (e.g. "0-3,24,25")
	CPUIDs string `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring the CPUs in this pool
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// ExclusiveCPUPoolStatus represents the status of exclusive CPU pools
// assigned to containers in a pod
type ExclusiveCPUPoolStatus struct {
	// PodUID is the UID of the pod (SSA map key)
	PodUID string `json:"podUID"`

	// Pod is the name of the pod
	Pod string `json:"pod"`

	// PowerContainers contains information about the containers using exclusive CPUs
	PowerContainers []PowerContainer `json:"powerContainers"`
}

// PowerContainer contains information about a container using exclusive CPUs
type PowerContainer struct {
	// Name is the name of the container
	Name string `json:"name"`

	// ID is the ID of the container
	ID string `json:"id"`

	// PowerProfile is the name of the PowerProfile applied to this container
	PowerProfile string `json:"powerProfile"`

	// CPUIDs are the CPU IDs assigned to the container
	CPUIDs []uint `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring the container
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// NodeUncoreStatus represents the status of uncore frequency configuration on a node
type NodeUncoreStatus struct {
	// Name is the name of the uncore frequency configuration
	Name string `json:"name"`

	// Config is the configuration of the uncore frequency configuration
	Config string `json:"config"`

	// Errors contains any errors encountered while configuring uncore frequency
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pns

// PowerNodeState is the Schema for the powernodestates API
// It provides per-node status for power management configuration
type PowerNodeState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PowerNodeStateSpec   `json:"spec,omitempty"`
	Status PowerNodeStateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PowerNodeStateList contains a list of PowerNodeState
type PowerNodeStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PowerNodeState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PowerNodeState{}, &PowerNodeStateList{})
}
