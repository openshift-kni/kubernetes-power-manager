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
	"k8s.io/apimachinery/pkg/types"
)

type StatusErrors struct {
	Errors []string `json:"errors,omitempty"`
}

type GuaranteedPod struct {
	// The name of the Node the Pod is running on
	Node string `json:"node,omitempty"`

	// The name of the Pod
	Name string `json:"name,omitempty"`

	Namespace string `json:"namespace,omitempty"`

	// The UID of the Pod
	UID string `json:"uid,omitempty"`

	// The Containers that are running in the Pod
	Containers []Container `json:"containers,omitempty"`
}

type Container struct {
	// The name of the Container
	Name string `json:"name,omitempty"`

	// The ID of the Container
	Id string `json:"id,omitempty"`

	// The name of the Pod the Container is running on
	Pod string `json:"pod,omitempty"`

	// The UID of the Pod the Container is running on
	PodUID types.UID `json:"podUID,omitempty"`

	// The exclusive CPUs given to this Container
	ExclusiveCPUs []uint `json:"exclusiveCpus,omitempty"`

	// The PowerProfile that the Container is utilizing
	PowerProfile string `json:"powerProfile,omitempty"`
}
