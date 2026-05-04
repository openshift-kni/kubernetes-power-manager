# Dynamic CPU Frequency Scaling for DPDK Polling Workloads

## Overview

- This feature dynamically adjusts CPU frequency for DPDK polling applications based on the DPDK usage metric. The Kubernetes Power Manager (KPM) samples per‑CPU
usage exposed by the DPDK telemetry socket, computes a windowed utilization for each CPU, and steers the CPU frequency to keep that utilization near a target band.
- Actuation is done through the Linux cpufreq subsystem using the `userspace` governor, which allows KPM to set explicit target frequencies.

## How it works

### Configuration parameters

Scaling behavior is configured in the `PowerProfile` under `spec.cpuScalingPolicy`. For example:

```yaml
apiVersion: power.cluster-power-manager.github.io/v1alpha1
kind: PowerProfile
metadata:
  name: dpdk-scaling
spec:
  pstates:
    governor: userspace
    min: 0%
    max: 100%
  cpuScalingPolicy:
    workloadType: polling-dpdk
    samplePeriod: 20ms
    cooldownPeriod: 60ms
    targetUsage: 80
    allowedUsageDifference: 5
    allowedFrequencyDifference: 10
    scalePercentage: 100
    fallbackFreqPercent: 0
```

- `workloadType`: must be `polling-dpdk` to enable this feature for a profile.
- `samplePeriod`: interval at which the agent samples usage and decides whether to adjust CPU frequency
- `cooldownPeriod`: waiting time after a frequency change before another adjustment for the same CPU is considered.
- `targetUsage`: desired usage percentage for each managed CPU.
- `allowedUsageDifference`: deadband around the target; when usage is within this band, the scaler holds the current target.
- `allowedFrequencyDifference`: minimum step (MHz) required to actually apply a computed change.
- `scalePercentage`: proportional gain (10–200). Higher values react more aggressively to error.
- `fallbackFreqPercent`: target frequency percentage when a usage sample is not available.

### Frequency adjustment flow

1. A guaranteed pod requesting a `PowerProfile` with `workloadType: polling-dpdk` is reconciled by the PowerPod controller.
2. The controller ensures a telemetry worker is created per DPDK pod to connect to its telemetry socket and sample cumulative `busy/total` cycle counters at a fixed internal window (≈10 ms). The worker computes windowed usage per CPU and stores the latest sample in a thread‑safe map.
3. For each managed CPU, a scaling worker evaluates control every `samplePeriod`:
   - Reads the latest windowed usage.
   - If usage is within `targetUsage ± allowedUsageDifference`, hold the current target.
   - Otherwise, compute a new target with the proportional rule below, clamp to limits, and apply if it exceeds `allowedFrequencyDifference`.
   - After applying, wait `cooldownPeriod` before evaluating that CPU again.

#### Windowed usage computation

The `/eal/lcore/usage` endpoint exposes cumulative counters. To base decisions on recent activity, the telemetry worker derives a per‑CPU windowed usage over its fixed sampling window.

`total_cycles[i]` and `busy_cycles[i]` are cumulative. On each poll, compute deltas against the previous sample:

```console
delta_total = total_cycles[i]_now - total_cycles[i]_prev
delta_busy  = busy_cycles[i]_now  - busy_cycles[i]_prev
delta_usage% = 100 * delta_busy / delta_total   # clamped to [0, 100]
```

#### Frequency update formula

```console
nextTargetFrequency = currentFrequency * (1 + (currentUsage / targetUsage - 1) * scalePercentage)
```

Then apply filters and limits:

- If `|nextTargetFrequency - previousTarget| < allowedFrequencyDifference`, hold the previous target.
- If usage is within the allowed range, hold the previous target.
- Clamp to configured min/max (`pstates.min/max`).
- Set the new target via cpufreq `userspace`, then wait `cooldownPeriod` before reevaluating that CPU.

## Testing and monitoring

1. Apply a PowerProfile with a scaling policy (see example above) to the cluster where you will run the DPDK app.

2. Launch the example DPDK server/client pair:

    ```console
    ./testbin/dpdk-testapp.sh [--non-siblings] [--replicas N]
    ```

    - The script deploys [examples/example-dpdk-testapp.yaml](../examples/example-dpdk-testapp.yaml) and starts two `dpdk-testpmd` instances in each pod: a client (traffic generator) and a server (receiver/forwarder). The scaler manages the CPUs assigned to the server container.
    - If Hyper-Threading is enabled, pass `--non-siblings` to pin the server to one logical CPU per physical core. If HT is disabled, omit the flag.
    - To launch multiple DPDK pods, pass `--replicas N` (default: 1). Each pod gets its own DPDK telemetry connection and CPU scaling.

3. Monitor frequency and usage on the node with [kpmon.py](../testbin/kpmon.py). Copy the script to the target node and run it there, as it reads CPU sysfs and MSR data locally:

    ```sh
    # On the target node:
    export KUBECONFIG=<path-to-kubeconfig>
    NODE=$(hostname)
    POD="<pod-name>"
    CPUS=$(oc get powernodestate "$NODE-power-state" -n power-manager -o jsonpath="{range .status.cpuPools.exclusive[?(@.pod==\"$POD\")]}{range .powerContainers[?(@.name==\"server\")]}{.cpuIDs}{end}{end}" | tr -d '[]')
    PODID=$(oc get powernodestate "$NODE-power-state" -n power-manager -o jsonpath="{range .status.cpuPools.exclusive[?(@.pod==\"$POD\")]}{.podUID}{end}")
    ./kpmon.py --cpu "$CPUS" --dpdk-pod-uid "$PODID" [--no-siblings] [--scroll]
    ```

4. To tear down the DPDK test app:

    ```console
    ./testbin/dpdk-testapp.sh -d
    ```
