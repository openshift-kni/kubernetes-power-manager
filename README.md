# Kubernetes Power Manager

> **⚠️ Notice:** A disruptive enhancement for multi-node cluster support is currently in progress on the `main` branch. For a stable codebase, please use the [v0.0.1](https://github.com/openshift-kni/kubernetes-power-manager/tree/v0.0.1) release tag.

This is an experimental project based on https://github.com/intel/kubernetes-power-manager (which has been
discontinued). It also includes enhancements from https://github.com/AMDEPYC/kubernetes-power-manager
(e.g. Dynamic CPU Frequency Management based on DPDK telemetry).

## Introduction

The Kubernetes Power Manager is a Kubernetes Operator that has been developed to provide cluster users with a
Kubernetes native mechanism to configure power management settings (e.g. c-states, p-states, uncore) through CRDs.
The main features include the ability to:
- Configure per-CPU power management (p-states, c-states) for reserved CPUs, shared CPUs and application workload
  CPUs (guaranteed pods) independently.
- Modify per-CPU power management configuration at runtime (for reserved, shared or application CPUs).
- Configure processor level power management (e.g. uncore frequency for Intel processors).
- Modify p-states for guaranteed pod CPUs running DPDK applications based on DPDK metrics.

The Kubernetes Power Manager supports Intel, AMD and ARM processor architectures. Modern processors give users more
precise control over CPU performance and power use on a per-core basis. Yet, Kubernetes is purposefully built to
operate as an abstraction layer between the workload and such hardware capabilities as a workload orchestrator. Users
of Kubernetes who are running performance-critical workloads with particular requirements reliant on hardware
capabilities encounter a challenge as a consequence.

The Kubernetes Power Manager bridges the gap between the container orchestration layer and hardware features enablement.

### Kubernetes Power Manager main responsibilities

- The Kubernetes Power Manager consists of two main components:
  - the overarching manager which is deployed anywhere on a cluster
  - the power node agent which is deployed on each node you require power management capabilities.
- The overarching operator is responsible for the configuration and deployment of the power node agent, while the power
  node agent is responsible for the tuning of the cores as requested by the user.

### Use Cases

- *Power Optimization over Performance.*
  A user may be interested in fast response time, but not in maximal response time, so may choose to spin up cores on
  demand but want to remain in power-saving mode the rest of the time. This can be done by configuring min/max CPU
  frequency ranges and a CPUFreq governor that adjusts the frequency based on CPU usage. In addition, selected c-states
  can be enabled to provide additional power savings when a CPU is idle.
- *Performance over Power Optimization.*
  A user may only be interested in fast response time, so may choose to have cores running at a high frequency at all
  times and disable most or all c-states to avoid any latency penalties from waking a CPU.
- *Mixed use.* A user may have a combination of applications - some of which demand the highest performance and
  response time, and some that are more concerned with power optimization.

The Kubernetes Power Manager supports all of the above use cases.

> Further Info:  Please see the *diagrams-docs* directory for diagrams with a visual breakdown of the power manager and its components.

## Functionality of the Kubernetes Power Manager

- **Frequency Tuning**

  Frequency tuning allows the individual cores on the system to be sped up or slowed down by changing their frequency.
  This tuning is done via the [Power Optimization Library](./power-optimization-library) which is now part of the project. More details in the [kernel CPU Performance Scaling section](https://docs.kernel.org/admin-guide/pm/cpufreq.html#cpu-performance-scaling).

  - **`scaling_min_freq` and `scaling_max_freq`**

    - The `min` and `max` values for a core are defined in the `PowerProfile` and the tuning is done after the core has been assigned by the Native CPU Manager.
    - The min/max frequency can be specified as an absolute value (in kHz) or as a linerar interpolation of hardware max and hardware min: `value = <hardware_min> + (hardware_max - hardware_min) * X%)`.
    - If min/max are not specified, hardware defaults will be used.The frequency of the cores are changed by writing the new frequency value to the `/sys/devices/system/cpu/cpuN/cpufreq/scaling_max|min_freq` file for the given core.

  - **Scaling Drivers**

    The following scaling drivers are currently supported in KPM:

    - **intel_pstate**
  
      Modern Intel CPUs automatically employ the `intel_pstate` CPU power scaling driver. This driver is integrated rather
      than a module, giving it precedence over other drivers. For Sandy Bridge and newer CPUs, this driver is currently
      used automatically. The BIOS P-State settings might be disregarded by Intel P-State.
      The Intel P-State driver utilizes the **Performance** and **Powersave** governors.

      - ***Performance***: The CPUfreq governor `performance` sets the CPU statically to the highest frequency within the borders of `scaling_min_freq` and `scaling_max_freq`.
      - ***Powersave***: The CPUfreq governor `powersave` sets the CPU statically to the lowest frequency within the borders of `scaling_min_freq` and `scaling_max_freq`.

    - **acpi-cpufreq**

      The acpi-cpufreq driver setting operates much like the P-state driver but has a different set of available
      governors. For more information see [here](https://www.kernel.org/doc/html/v4.12/admin-guide/pm/cpufreq.html).

      One thing to note is that acpi-cpufreq reports the base clock as the frequency hardware limits however the P-state
      driver uses turbo frequency limits.
      Both drivers can make use of turbo frequency; however, acpi-cpufreq can exceed hardware frequency limits when using
      turbo frequency.
      This is important to take into account when setting frequencies for profiles.

    - **intel_cpufreq**

    - **amd-pstate**

    - **amd-pstate-epp**
  
    - **cppc_cpufreq**

      This is often the default for aarch64 systems.

  - **Energy Performance Preference (EPP)**

    The user can arrange cores according to priority levels using this capability. When the system has extra power, it can
    be distributed among the cores according to their priority level. Although it cannot be guaranteed, the system will
    try to apply the additional power to the cores with the highest priority. This feature requires support from both
    the underlying processor and the scaling driver.
    There are four levels of priority available:

      1. Performance
      2. Balance Performance
      3. Balance Power
      4. Power

    The Priority level for a core is defined using its EPP (Energy Performance Preference) value, which is one of the
    options in the Power Profiles. If not all the power is utilized on the CPU, the CPU can put the higher priority cores
    up to Turbo Frequency (allows the cores to run faster).

- **CPU Idle Time Management**

  To save energy on a system, you can allow the CPU to go into a low-power mode. Each CPU has several power modes, which are collectively called C-States. These work by cutting the clock signal and power from idle CPUs, or CPUs that are not executing commands. More details in the [kernel CPU Idle section](https://docs.kernel.org/driver-api/pm/cpuidle.html).
  
  KPM supports both explicit C-state configuration by name and latency-based C-states configuration, allowing for a more fine-grained control over the trade-off between power saving and latency. C-states can now be configured directly within PowerProfiles alongside P-state settings.

  - The C-States configuration in Linux is stored in `/sys/devices/system/cpu/cpuN/cpuidle` or `/sys/devices/system/cpu/cpuidle`. To determine the driver in use, simply check the `/sys/devices/system/cpu/cpuidle/current_driver` file.
  - Before configuring C-states in a PowerProfile, the user must confirm which C-states are actually available on the system. The available C-States are found under `/sys/devices/system/cpu/cpuN/cpuidle/stateN/`.

- **Uncore and equivalents**

  - **Intel**

    The largest part of modern CPUs is outside the actual cores. On Intel CPUs this is part is called the "Uncore" and has
    last level caches, PCI-Express, memory controller, QPI, power management and other functionalities.
    The previous deployment pattern was that an uncore setting was applied to sets of servers that are allocated as
    capacity for handling a particular type of workload. This is typically a one-time configuration today. The Kubenetes
    Power Manager now makes this dynamic and through a cloud native pattern. The implication is that the cluster-level
    capacity for the workload can then configured dynamically, as well as scaled dynamically. Uncore frequency applies to
    Xeon scalable and D processors could save up to 40% of CPU power or improved performance gains.

  - **AMD**

    Unlike Intel, AMD does not expose uncore frequency controls (such as LLC, memory controller, or fabric clocks) via a
    standard kernel interface. There is no equivalent to Intel’s `intel_uncore_frequency` driver or
    `/sys/devices/system/cpu/intel_uncore_frequency` sysfs interface.

    Instead, uncore frequency management on AMD EPYC platforms is supported via the
    [ESMI library](https://github.com/amd/esmi_ib_library/tree/master), a user-space interface that communicates with
    hardware using the amd_hsmp kernel driver.
    > Note: The *amd_hsmp* driver might not be loaded by default and must be manually enabled:
    >
    > ```console
    > sudo modprobe amd_hsmp
    > ```

    In ADM KPM, the following logic applies when configuring DF P-states:
    - When min equals max, a fixed DF P-state is set. This disables automatic DF p-state scaling and locks the DF to operate at that specific performance level.
    - When min differs max, DF is allowed to dynamically scale between the specified DF P-states range.

    This is not currently supported in KPM.

  - **ARM** - no equivalent supported

## Prerequisites

- **Node Feature Discovery** ([NFD](https://github.com/kubernetes-sigs/node-feature-discovery)) should be deployed in
  the cluster before running the Kubernetes Power Manager.
  NFD is used to detect node-level features such as *Intel Speed Select Technology - Base Frequency (SST-BF)*.
  Once detected, the user can instruct the Kubernetes Power Manager to deploy the Power Node Agent to Nodes with
  SST-specific labels, allowing the Power Node Agent to take advantage of such features by configuring cores on the
  host to optimise performance for containerized workloads.
  
  > **Note: NFD is recommended, but not essential. Node labels can also be applied manually. See
  the [NFD repo](https://github.com/kubernetes-sigs/node-feature-discovery#feature-labels) for a full list of features
  labels.**

- If not using NFD or labels added through NFD, label the node manually with a label of your choosing:

  ```console
  kubectl label node <node-name> feature.node.kubernetes.io/power-node=true
  ```

  > Note: Make sure to use the same label in the `PowerConfig`, under `spec.powerNodeSelector`.

- **Important**: In the kubelet configuration file the `cpuManagerPolicy` has to set to `static`, and the
  `reservedSystemCPUs` must be set to the desired value (full file [here](./examples/example-kubelet-configuration.yaml)):

  ```yaml
  apiVersion: kubelet.config.k8s.io/v1beta1
  ...
  cpuManagerPolicy: "static"
  ...
  reservedSystemCPUs: "0"
  ...
  ```

## Deploying the Kubernetes Power Manager using kustomize

- Build the 2 images:

  ```console
  IMG_AGENT=quay.io/<user/org>/power-node-agent:latest IMG=quay.io/<user/org>/power-operator:latest IMGTOOL=<docker/podman> make update

  OCP=true IMG_AGENT=quay.io/<user/org>/power-node-agent:latest IMG=quay.io/<user/org>/power-operator:latest IMGTOOL=<docker/podman> make images-ocp
  ```

  > **Note**: By default, the images are built for x86_64 platforms. For an ARM platform, add the `PLATFORM=linux/arm64` parameter:
  >
  > ```console
  > OCP=true IMG_AGENT=<...> IMG=<...> PLATFORM=linux/arm64 IMGTOOL=<...> make images-ocp
  > ```

- Push the 2 images:

  ```console
  <docker/podman> push <image>
  ```

- Install the CRDs and deploy the operator:

  ```console
  OCP=true IMG_AGENT=quay.io/<user/org>/power-node-agent:latest IMG=quay.io/<user/org>/power-operator:latest make install deploy
  ```

## Building Multi-Architecture Images

The Kubernetes Power Manager supports building multi-architecture container images for both the Power Operator and Power Node Agent. This allows you to create a single image tag that works across multiple architectures (e.g., AMD64 and ARM64).

### Prerequisites

**For Podman:**

Podman 3.0+ with native multi-arch support:

```console
# Verify podman version (3.0+ required)
podman version

# On Linux: Ensure qemu-user-static is installed for cross-platform builds
sudo apt-get install qemu-user-static  # Debian/Ubuntu
sudo dnf install qemu-user-static      # Fedora/RHEL

# On macOS: Initialize and start podman machine
# Make sure Rosetta is enabled when building on Apple Silicon
podman machine init # customize cpus, disk-size, memory if required
podman machine start
```

**For Docker:**

Docker buildx must be installed and configured:

```console
# Check if buildx is available
docker buildx version

# Create a new builder instance (one-time setup)
docker buildx create --name multiarch --use
docker buildx inspect --bootstrap
```

### Build Multi-Arch Images

Build both operator and agent images for multiple architectures (default: linux/amd64 and linux/arm64):

```console
# Using Podman
IMGTOOL=podman \
IMAGE_REGISTRY=quay.io/<user/org> \
VERSION=latest \
make build-push-multiarch

# Using Docker
IMGTOOL=docker \
IMAGE_REGISTRY=quay.io/<user/org> \
VERSION=latest \
make build-push-multiarch
```

For OpenShift (uses UBI base image and OCP-specific manifests):

```console
OCP=true \
IMGTOOL=podman \
IMAGE_REGISTRY=quay.io/<user/org> \
VERSION=latest \
make build-push-multiarch
```

### Customizing Target Platforms

Override the default platforms (linux/amd64,linux/arm64):

```console
PLATFORMS=linux/amd64,linux/arm64,linux/arm/v7 \
IMGTOOL=podman \
IMAGE_REGISTRY=quay.io/<user/org> \
VERSION=latest \
make build-push-multiarch
```

### Verifying Multi-Arch Images

After pushing, verify the multi-arch manifest:

```console
# Using Podman
podman manifest inspect quay.io/<user/org>/kubernetes-power-manager-operator:latest

# Using Docker
docker buildx imagetools inspect quay.io/<user/org>/kubernetes-power-manager-operator:latest
```

## Deploying the Kubernetes Power Manager using Helm

The Kubernetes Power Manager includes a helm chart for the latest releases, allowing the user to easily deploy
everything that is needed for the overarching operator and the node agent to run. The following versions are
supported with helm charts:

- TODO

When set up using the provided helm charts, the following will be deployed:

- The power-manager namespace
- The RBAC rules for the operator and node agent
- The operator deployment itself
- The operator's power config
- A shared power profile

To change any of the values the above are deployed with, edit the values.yaml file of the relevant helm chart.

To deploy the Kubernetes Power Manager using Helm, you must have Helm installed. For more information on installing
Helm, see the installation guide [here](https://helm.sh/docs/intro/install/).

To install the latest version, use the following command:

`make helm-install`

To uninstall the latest version, use the following command:

`make helm-uninstall`

You can use the HELM_CHART and OCP parameters to deploy an older or Openshift specific version of the Kubernetes Power Manager:

`HELM_CHART=v2.3.1 OCP=true make helm-install`
`HELM_CHART=v2.2.0 make helm-install`
`HELM_CHART=v2.1.0 make helm-install`

Please note when installing older versions that certain features listed in this README may not be supported.

## Components

### Power Optimization Library

The [Power Optimization Library](./power-optimization-library) takes the desired configuration
for the cores associated with Exclusive Pods and tunes them based on the requested `PowerProfile`. The Power Optimization
Library will also facilitate the use of C-States functionality.

### Power Node Agent

The Power Node Agent is also a containerized application deployed by the Kubernetes Power Manager in a DaemonSet.

The primary function of the node agent is to communicate with the node's Kubelet PodResources endpoint to discover the
exact cores that are allocated per container. The node agent watches for Pods that are created in your cluster and examines
them to determine which `PowerProfile` they have requested and then sets off the chain of events that tunes the
frequencies of the cores designated to the Pod.

### Power Config controller

The Kubernetes Power Manager will wait for the `PowerConfig` CR to be created by the user to initiate the deployment of
the node agent. The `PowerConfig` specifies what Nodes the user wants to place the node agent on.

> `spec.powerNodeSelector`: This is a key/value map used for defining a list of node labels that a node must satisfy in order for the Power Node Agent to be deployed.

Once the Power Config controller sees that the `PowerConfig` is created, it deploys the power node agent on each of the
Nodes that are specified. `PowerProfiles` should be created separately by the user and are advertised as
extended resources that can be requested in the PodSpec. The Kubelet can then keep track of these requests.
The extended resources can control how many cores on the system can be run at a higher frequency and help avoid hitting
the heat threshold which would limit frequencies.

**Note**: Only one `PowerConfig` can be present in a cluster. The Config Controller will ignore and delete and subsequent
PowerConfigs created after the first.

**Example:**

```yaml
apiVersion: "power.cluster-power-manager.github.io/v1alpha1"
kind: PowerConfig
metadata:
  name: power-config
  namespace: power-manager
spec:
  powerNodeSelector:
    feature.node.kubernetes.io/power-node: "true"
```

### Power Node Config controller

The Power Node Config controller is responsible for configuring the shared and reserved CPU pools on each node. It uses
the Power Optimization Library to set the power profile on the shared pool, move CPUs between the shared and reserved
pools, and apply frequency/power settings to reserved CPUs.

The `PowerNodeConfig` CR is created by the user to define the shared pool configuration for nodes matching its
`spec.nodeSelector`. The shared pool profile tunes all cores on the system (except reserved CPUs) to the specified
frequency settings. Reserved CPUs can optionally be configured with their own `PowerProfile`.

> **Note: A `PowerNodeConfig` is mandatory if guaranteed pods will need to use `PowerProfiles`.**

Reserved CPUs can optionally be specified in the `PowerNodeConfig` to exclude them from shared pool frequency tuning,
since these are typically used by Kubernetes system processes.
If specified, the `reservedCPUs` values should correspond to the `reservedSystemCPUs` in the user’s Kubelet config.

**Example:**

```yaml
apiVersion: "power.cluster-power-manager.github.io/v1alpha1"
kind: PowerNodeConfig
metadata:
  name: shared-example-config
  namespace: power-manager
spec:
  sharedPowerProfile: shared
  nodeSelector:
    labelSelector:
      matchLabels:
        node-role.kubernetes.io/worker: ""
  reservedCPUs:
  - cores:
    - 0
    - 1
    - 24
    - 25
    powerProfile: performance
```

### Power Profile Controller

The Power Profile controller holds values for specific settings which are then applied to cores at host level by the
Kubernetes Power Manager as requested. `PowerProfiles` are advertised as extended resources and can be requested via the
PodSpec. All `PowerProfiles` must be created explicitly by the user.

**Example:**

```yaml
apiVersion: "power.cluster-power-manager.github.io/v1alpha1"
kind: PowerProfile
metadata:
  name: performance-example-application
spec:
  cpuCapacity: "75%"
  nodeSelector:
    labelSelector:
      matchExpressions:
      - key: test
        operator: In
        values:
        - test
  pstates:
    max: 3700
    min: 3300
    epp: "performance"
  cstates:
  # maxLatencyUs: 100
    names:
      C1: true
      C6: false
```

Note:
- `spec.pstates.min` and `spec.pstates.max` can hold both scalar and percentage values.
- `spec.cpuCapacity` has been added to configure the node's CPU capacity. It can hold both scalar and percentage values.
- `spec.nodeSelector` can be used to choose to which node the `PowerProfile` applies to.
- The PowerProfile CRD has been enhanced to support both P-states (frequency) and C-states (power saving) configuration in
  a single, unified structure. C-states can be configured either by explicit state names or by maximum latency threshold
  for more flexible power tuning across different CPU architectures.
- The `spec.pstates.epp` only applies to processors that support it.

Dynamic scaling for DPDK polling workloads is also supported via `spec.cpuScalingPolicy`.
See [Dynamic CPU Frequency Scaling for DPDK workloads](docs/dpdk-dynamic-scaling.md) for details.

A Shared `PowerProfile` must also be created by the user and referenced by the `PowerNodeConfig`'s `spec.sharedPowerProfile`
field. The Power Profile controller determines that a `PowerProfile` is shared through the `spec.shared: true` parameter.
The same shared `PowerProfile` can be referenced by multiple `PowerNodeConfig` CRs targeting different nodes.

**Examples:**

```yaml
apiVersion: "power.cluster-power-manager.github.io/v1alpha1"
kind: PowerProfile
metadata:
  name: shared-example-node1
spec:
  shared: true
  pstates:
    max: 1500
    min: 1000
    epp: "power"
    governor: "powersave"
```

```yaml
apiVersion: "power.cluster-power-manager.github.io/v1alpha1"
kind: PowerProfile
metadata:
  name: shared-example-node2
spec:
  cpuCapacity: "75%"
  nodeSelector:
    labelSelector:
      matchExpressions:
      - key: test
        operator: In
        values:
        - test
  shared: true
  pstates:
    max: "50%" # scalar is also accepted; if missing, it defaults to the hardware max
    min: "20%" # scalar is also accepted; if missing, it defaults to the hardware min
    governor: "powersave"
  # Names or maxLatencyUs, only one is supported.
  cstates:
  # maxLatencyUs: 100
    names:
      C1: true
      C6: false
```

> **Note: Non-Shared `PowerProfiles` are advertised as extended resources on matching nodes and can be
requested by guaranteed pods via the PodSpec.**

### PowerNodeState

The `PowerNodeState` CR provides observability into the node's power management operations. It is automatically managed
by the controllers and displays the current state of PowerProfiles, CPU pool configurations, and guaranteed pod
container assignments.

The `PowerNodeState` status is updated via Server-Side Apply (SSA) by the PowerProfile, PowerPod, and PowerNodeConfig
controllers, each owning their respective fields.

**Example:**

```yaml
apiVersion: power.cluster-power-manager.github.io/v1alpha1
kind: PowerNodeState
metadata:
  name: example-node-power-state
  namespace: power-manager
spec: {}
status:
  cpuPools:
    exclusive:
    - pod: guaranteed-perf-pod-2
      podUID: 8fb49327-3e69-4b68-8e64-b21ab0660462
      powerContainers:
      - cpuIDs:
        - 2
        - 26
        id: cri-o://94d80f7e714933ebe2fc3aef8e9076aa6e8d4ce7c026e961f9547515ce9d8aa2
        name: app-container
        powerProfile: application
      - cpuIDs:
        - 3
        id: cri-o://d085c76e40eb0e8e593e616611499e7331605b5d1d65c7d05280c5ea47788fda
        name: performance-container
        powerProfile: performance
    reserved:
    - powerNodeConfig: shared-performance
      powerProfileCPUs:
      - cpuIDs: 0-1,24-25
        powerProfile: performance
    shared:
      cpuIDs: 4-23,27-47
      powerNodeConfig: shared-performance
      powerProfile: shared
  powerProfiles:
  - config: 'Min: 3000, Max: 3900, Governor: powersave, EPP: , C-States: enabled:
      C1,C1E; disabled: C6,POLL'
    name: application
  - config: 'Min: 2800, Max: 3200, Governor: powersave, EPP: , C-States: enabled:
      C1,C1E,C6; disabled: POLL'
    name: balance-performance
  - config: 'Min: 3200, Max: 3400, Governor: performance, EPP: performance, C-States:
      enabled: C1; disabled: C1E,C6,POLL'
    name: performance
  - config: 'Min: 1000, Max: 1000, Governor: powersave, EPP: power, C-States: '
    name: shared
```

### Power Pod controller

The Power Pod Controller watches for pods. When a pod comes along the Power Pod Controller checks if the pod is in the
guaranteed quality of service class (using exclusive
cores, [see documentation](https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/), taking a core
out of the shared pool as it is the only option in Kubernetes that can do this operation). Then it examines the Pods to
determine which PowerProfile has been requested, moves the pod's CPUs into the corresponding exclusive pool in the Power
Optimization Library, and updates the `PowerNodeState` status.

> **Note**: the request and the limits must have a matching number of cores and are also on a container-by-container basis.
Currently the Kubernetes Power Manager supports multiple `PowerProfile` per Pod, but only one `PowerProfile` per container.

### Uncore Frequency - only applicable to Intel CPUs

[example-uncore.yaml](examples/example-uncore.yaml)

### Error handling

If any error occurs it will be displayed in the status field of the custom resource, for example:

```yaml
apiVersion: power.cluster-power-manager.github.io/v1alpha1
kind: PowerProfile
  ...
status:
  errors:
  - the PowerProfile CRD name must match name of one of the power nodes
```

If no errors occurred or were corrected, the list will be empty

```yaml
apiVersion: power.cluster-power-manager.github.io/v1alpha1
kind: PowerProfile
  ...
status:
  errors: []
```

## End to end workflow

1. Build and install KPM with either [Helm](#deploying-the-kubernetes-power-manager-using-helm) or [Kustomize](#deploying-the-kubernetes-power-manager-using-kustomize).

2. Label the nodes in accordance to the `spec.powerNodeSelector` field of the `PowerConfig` that will be applied.

3. Create the **Power Config** CR - use the example [PowerConfig](examples/example-powerconfig.yaml).

    ```console
    kubectl apply -f examples/example-powerconfig.yaml
    ```

    Once deployed, the controller-manager pod will see it via the PowerConfig controller and create a Node Agent instance on nodes specified with the `feature.node.kubernetes.io/power-node: "true"` label.

    The *power-node-agent* DaemonSet will be created, managing the Power Node Agent Pods.

4. Create the **Shared PowerProfile** - use the example Shared [PowerProfile](examples/example-shared-profile.yaml).

    ```console
    kubectl apply -f examples/example-shared-profile.yaml
    ```

5. If needed, create other non-Shared `PowerProfiles`.
    > Note: These would usually be used for reserved or exclusive CPUs.

6. Create the **PowerNodeConfig** to configure shared and reserved CPU pools.

    Replace the necessary values with those that correspond to your cluster and apply:

    ```console
    kubectl apply -f examples/example-powernodeconfig.yaml
    ```


    - Once applied, the PowerNodeConfig controller will configure the shared and reserved CPU pools.
    - All of the cores on the system except for the reservedCPUs will be tuned to the shared profile's frequency settings.
    - The reservedCPUs will be kept at the system default min and max frequency by default. If the user specifies a
     `PowerProfile` along with a set of reserved
      cores then a separate pool will be created for those cores and that profile. If an invalid profile is supplied the
      cores will instead be placed in the default reserved pool with system defaults.
        > Note: In most instances, leaving these cores at system defaults is the best approach to prevent important k8s or kernel related processes from becoming starved.

7. Create the **Performance Pod(s)** - use the example [Pod](examples/example-pod.yaml).

    Replace the placeholder values with the `PowerProfile` you require and apply:

    ```console
    kubectl apply -f examples/example-pod.yaml
    ```

    At this point, the cluster will contain the PowerProfiles you created:

    ```console
    # kubectl get powerprofiles -n power-manager
    NAME                          AGE
    performance                   59m
    shared-example                60m
    ```

    Check the `PowerNodeState` status for a summary of the CPU pool configuration on each node.

8. **Delete Pods**

    ```console
    kubectl delete pods <name>
    ```

    When a Pod with exclusive CPU assignments is deleted, the cores associated with that Pod are moved back to the
    shared pool and returned to the shared profile's frequencies. The `PowerNodeState` status is updated accordingly.
