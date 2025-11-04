// this file contains integration tests pof the power library
package power

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// this test checks for potential race condition where one go routine moves cpus to a pool and another changes a power
// profile of the target pool
func TestConcurrentMoveCpusSetProfile(t *testing.T) {
	const count = 5
	for i := 0; i < count; i++ {
		doConcurrentMoveCPUSetProfile(t)
	}
	// reset feature list
	for _, status := range featureList {
		status.err = uninitialisedErr
	}
}

func doConcurrentMoveCPUSetProfile(t *testing.T) {
	const numCpus = 88
	emin := "11100"
	emax := "5550000"
	cpuConfig := map[string]string{
		"min":                 "11100",
		"max":                 "9990000",
		"driver":              "intel_pstate",
		"available_governors": "performance",
		"epp":                 "performance",
	}

	ecoreConfig := map[string]string{}
	maps.Copy(ecoreConfig, cpuConfig)
	ecoreConfig["min"] = emin
	ecoreConfig["max"] = emax

	cpuConfigAll := map[string]map[string]string{}

	cpuTopologyMap := map[string]map[string]string{}
	cpuCstatesMap := map[string]map[string]map[string]string{}
	for i := 0; i < numCpus; i++ {
		// set e cores
		if i > numCpus/2 {
			cpuConfigAll[fmt.Sprint("cpu", i)] = ecoreConfig
		} else {
			// set p cores
			cpuConfigAll[fmt.Sprint("cpu", i)] = cpuConfig
		}

		// set c-states
		cpuCstatesMap[fmt.Sprint("cpu", i)] = map[string]map[string]string{
			"state0": {"name": "POLL", "disable": "0", "latency": "0"},
			"state1": {"name": "C1", "disable": "0", "latency": "1"},
			"state2": {"name": "C1E", "disable": "0", "latency": "10"},
			"state3": {"name": "C6", "disable": "0", "latency": "170"},
		}
		cpuCstatesMap["Driver"] = map[string]map[string]string{"intel_idle\n": nil}

		// for this test we don't care about topology, so we just emulate 1 pkg, 1 die, numCpus cores, no hyperthreading
		cpuTopologyMap[fmt.Sprint("cpu", i)] = map[string]string{
			"pkg":  "0",
			"die":  "0",
			"core": fmt.Sprint(i),
		}
	}

	// Setup features except for uncore
	defer setupCpuCStatesTests(cpuCstatesMap)()
	defer setupIntelUncoreTests(map[string]map[string]string{}, "")()
	defer setupCpuScalingTests(cpuConfigAll)()
	defer setupTopologyTest(cpuTopologyMap)()

	originalGetFromLscpu := GetFromLscpu
	defer func() { GetFromLscpu = originalGetFromLscpu }()
	GetFromLscpu = TestGetFromLscpu

	instance, err := CreateInstance("host")
	assert.ErrorContainsf(t, err, "intel_uncore_frequency not loaded", "expecting uncore feature error")
	assert.NotNil(t, instance)

	assert.Len(t, *instance.GetAllCpus(), numCpus)
	assert.ElementsMatch(t, *instance.GetReservedPool().Cpus(), *instance.GetAllCpus())
	assert.Empty(t, *instance.GetSharedPool().Cpus())

	powerProfile, err := NewPowerProfile("pwr", &intstr.IntOrString{Type: intstr.Int, IntVal: 100}, &intstr.IntOrString{Type: intstr.Int, IntVal: 1000}, "performance", "performance", map[string]bool{"C1": true, "C6": false}, nil)
	assert.NoError(t, err)

	moveCoresErrChan := make(chan error)
	setPowerProfileErrChan2 := make(chan error)

	go func(instance Host, errChannel chan error) {
		errChannel <- instance.GetSharedPool().MoveCpus(*instance.GetAllCpus())
	}(instance, moveCoresErrChan)

	go func(instance Host, profile Profile, errChannel chan error) {
		time.Sleep(5 * time.Millisecond)
		errChannel <- instance.GetSharedPool().SetPowerProfile(profile)
	}(instance, powerProfile, setPowerProfileErrChan2)

	assert.NoError(t, <-moveCoresErrChan)
	close(moveCoresErrChan)

	assert.NoError(t, <-setPowerProfileErrChan2)
	close(setPowerProfileErrChan2)

	assert.Equal(t, powerProfile, instance.GetSharedPool().GetPowerProfile())
	assert.ElementsMatch(t, *instance.GetAllCpus(), *instance.GetSharedPool().Cpus())
	for i := uint(0); i < numCpus; i++ {
		assert.NoError(t, verifyPowerProfile(i, powerProfile), "cpuid", i)
	}
}

// verifies that the cpu is configured correctly
// checking is done relative to basePath
func verifyPowerProfile(cpuId uint, profile Profile) error {
	var allerrs []error
	var err error

	pstates := profile.GetPStates()
	governor, err := readCpuStringProperty(cpuId, scalingGovFile)
	allerrs = append(allerrs, err)
	if governor != pstates.GetGovernor() {
		allerrs = append(allerrs, fmt.Errorf("governor mismatch expected : %s, current %s", pstates.GetGovernor(), governor))
	}

	if pstates.GetEpp() != "" {
		epp, err := readCpuStringProperty(cpuId, eppFile)
		allerrs = append(allerrs, err)
		if epp != pstates.GetEpp() {
			allerrs = append(allerrs, fmt.Errorf("epp mismatch expected : %s, current %s", pstates.GetEpp(), epp))
		}
	}

	maxFreq, err := readCpuUintProperty(cpuId, scalingMaxFile)
	allerrs = append(allerrs, err)
	if maxFreq != uint(pstates.GetMaxFreq().IntVal) {
		allerrs = append(allerrs, fmt.Errorf("maxFreq mismatch expected %d, current %d", pstates.GetMaxFreq().IntVal, maxFreq))
	}
	minFreq, err := readCpuUintProperty(cpuId, scalingMinFile)
	allerrs = append(allerrs, err)
	if minFreq != uint(pstates.GetMinFreq().IntVal) {
		allerrs = append(allerrs, fmt.Errorf("minFreq mismatch expected %d, current %d", pstates.GetMinFreq().IntVal, minFreq))
	}

	for stateName, expected := range profile.GetCStates().States() {
		actual, err := readCpuStringProperty(cpuId, fmt.Sprintf(cStateDisableFileFmt, allCPUCStatesInfo[cpuId][stateName].StateNumber))
		allerrs = append(allerrs, err)

		if expected != (actual == "0") {
			allerrs = append(allerrs, fmt.Errorf("c-state %s mismatch expected %t, current %t", stateName, expected, actual == "0"))
		}
	}

	return errors.Join(allerrs...)
}
