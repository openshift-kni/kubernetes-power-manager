package power

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type mockUncore struct {
	mock.Mock
}

func (m *mockUncore) write(pkIgD, dieID uint) error {
	return m.Called(pkIgD, dieID).Error(0)
}

func setupIntelUncoreTests(files map[string]map[string]string, modulesFileContent string) func() {
	origBasePath := basePath
	basePath = "testing/cpus"

	originalCpuIdentity := cpuIdentity
	cpuIdentity.architecture = architectureX86_64
	cpuIdentity.vendorID = vendorIDIntel

	origModulesFile := kernelModulesFilePath
	kernelModulesFilePath = basePath + "/kernelModules"

	featureList[UncoreFeature].err = nil

	if err := os.MkdirAll(filepath.Join(basePath, intelUncoreDirName), os.ModePerm); err != nil {
		panic(err)
	}

	if modulesFileContent != "" {
		if err := os.WriteFile(kernelModulesFilePath, []byte(modulesFileContent), 0644); err != nil {
			panic(err)
		}
	}

	for pkgDie, freqFiles := range files {
		pkgUncoreDir := filepath.Join(basePath, intelUncoreDirName, pkgDie)
		if err := os.MkdirAll(filepath.Join(pkgUncoreDir), os.ModePerm); err != nil {
			panic(err)
		}
		for file, value := range freqFiles {
			switch file {
			case "initMax":
				if err := os.WriteFile(path.Join(pkgUncoreDir, intelUncoreInitMaxFreqFile), []byte(value), 0644); err != nil {
					panic(err)
				}
			case "initMin":
				if err := os.WriteFile(path.Join(pkgUncoreDir, intelUncoreInitMinFreqFile), []byte(value), 0644); err != nil {
					panic(err)
				}
			case "Max":
				if err := os.WriteFile(path.Join(pkgUncoreDir, intelUncoreMaxFreqFile), []byte(value), 0644); err != nil {
					panic(err)
				}
			case "Min":
				if err := os.WriteFile(path.Join(pkgUncoreDir, intelUncoreMinFreqFile), []byte(value), 0644); err != nil {
					panic(err)
				}
			}
		}
	}
	return func() {
		if err := os.RemoveAll(strings.Split(basePath, "/")[0]); err != nil {
			panic(err)
		}
		featureList[UncoreFeature].err = uninitialisedErr
		kernelModulesFilePath = origModulesFile
		basePath = origBasePath
		cpuIdentity = originalCpuIdentity

		defaultUncore = &uncoreFreq{}
	}
}

func TestNewUncore(t *testing.T) {
	var ucre Uncore
	var err error
	defer setupIntelUncoreTests(map[string]map[string]string{}, "")()

	// Intel
	// happy path
	defaultUncore.min = 1_200_000
	defaultUncore.max = 2_400_000

	ucre, err = NewUncore(1_400_000, 2_200_000)
	assert.NoError(t, err)
	assert.Equal(t, uint(1_400_000), ucre.(*uncoreFreq).min)
	assert.Equal(t, uint(2_200_000), ucre.(*uncoreFreq).max)

	// max too high
	ucre, err = NewUncore(1_400_000, 9999999)
	assert.Nil(t, ucre)
	assert.ErrorContains(t, err,
		"requested max uncore frequency (kHz) 9999999 is higher than 2400000 allowed by the hardware")

	// min too low
	ucre, err = NewUncore(100, 2_200_000)
	assert.Nil(t, ucre)
	assert.ErrorContains(t, err,
		"requested min uncore frequency (kHz) 100 is lower than 1200000 allowed by the hardware")

	// AMD
	defaultUncore.min = 0
	defaultUncore.max = 2
	cpuIdentity.vendorID = vendorIDAMD

	// AMD max too high
	ucre, err = NewUncore(1, 3)
	assert.Nil(t, ucre)
	assert.ErrorContains(t, err,
		"requested max DF P-state 3 is higher than 2 allowed by the hardware")
	// AMD max is lower than min
	ucre, err = NewUncore(2, 1)
	assert.Nil(t, ucre)
	assert.ErrorContains(t, err,
		"requested max DF P-state 1 cannot be lower than min DF P-state 2")

	//arm uncore not supported
	featureList[UncoreFeature].err = fmt.Errorf(
		"uncore feature is not supported on aarch64 architecture (Ampere vendor)")
	ucre, err = NewUncore(1, 2)
	assert.ErrorIs(t, err, featureList[UncoreFeature].err)
}

func Test_initUncore(t *testing.T) {
	var feature featureStatus
	var teardown func()
	teardown = setupIntelUncoreTests(map[string]map[string]string{
		"package_00_die_00": {
			"initMax": "999",
			"initMin": "100",
		},
	},
		"intel_cstates 14 0 - Live 0000ffffad212d\n"+
			intelUncoreKmodName+" 324 0 - Live 0000ffff3ea334\n"+
			"rtscan 2342 0 -Live 0000ffff234ab4d",
	)
	defer teardown()
	// happy path
	feature = initUncore()

	assert.Equal(t, "Uncore frequency", feature.name)
	assert.Equal(t, "N/A", feature.driver)

	assert.NoError(t, feature.err)
	assert.Equal(t, uint(999), defaultUncore.max)
	assert.Equal(t, uint(100), defaultUncore.min)
	teardown()

	// module not loaded
	teardown = setupIntelUncoreTests(map[string]map[string]string{},
		"intel_cstates 14 0 - Live 0000ffffad212d\n"+
			"rtscan 2342 0 -Live 0000ffff234ab4d",
	)
	feature = initUncore()
	assert.ErrorContains(t, feature.err, "not loaded")
	teardown()

	// no dies to manage
	teardown = setupIntelUncoreTests(map[string]map[string]string{},
		"intel_cstates 14 0 - Live 0000ffffad212d\n"+
			intelUncoreKmodName+" 324 0 - Live 0000ffff3ea334\n"+
			"rtscan 2342 0 -Live 0000ffff234ab4d",
	)
	feature = initUncore()
	assert.ErrorContains(t, feature.err, "empty or invalid")
	teardown()

	// cant read init freqs
	teardown = setupIntelUncoreTests(map[string]map[string]string{
		"package_00_die_00": {},
	},
		"intel_cstates 14 0 - Live 0000ffffad212d\n"+
			intelUncoreKmodName+" 324 0 - Live 0000ffff3ea334\n"+
			"rtscan 2342 0 -Live 0000ffff234ab4d",
	)
	feature = initUncore()
	assert.ErrorContains(t, feature.err, "failed to determine init freq")
	teardown()
}

func TestUncoreFreq_write(t *testing.T) {
	defer setupIntelUncoreTests(map[string]map[string]string{
		"package_00_die_00": {
			"Max": "999",
			"Min": "100",
		},
		"package_01_die_00": {
			"Max": "999",
			"Min": "100",
		},
	}, "")()

	uncore := uncoreFreq{min: 1, max: 9323}
	err := uncore.write(1, 0)
	assert.NoError(t, err)

	value, _ := readUncoreProperty(1, 0, intelUncoreMinFreqFile)
	assert.Equal(t, uint(1), value)

	value, _ = readUncoreProperty(1, 0, intelUncoreMaxFreqFile)
	assert.Equal(t, uint(9323), value)

	// write to non-existing file
	err = uncore.write(2, 3)
	assert.ErrorContains(t, err, "no such file or directory")
}

func TestCpuTopology_SetUncoreFrequency(t *testing.T) {
	uncore := &uncoreFreq{}
	pkg1 := new(mockCpuPackage)
	pkg1.On("applyUncore").Return(nil)
	topo := cpuTopology{
		packages: packageList{0: pkg1},
	}

	assert.NoError(t, topo.SetUncore(uncore))
	assert.Equal(t, uncore, topo.uncore)
	pkg1.AssertExpectations(t)
}

func TestCpuTopology_applyUncore(t *testing.T) {
	pkg1 := new(mockCpuPackage)
	pkg1.On("applyUncore").Return(nil)
	pkg2 := new(mockCpuPackage)
	pkg2.On("applyUncore").Return(nil)

	topo := &cpuTopology{packages: packageList{0: pkg1, 1: pkg2}}
	assert.NoError(t, topo.applyUncore())
	pkg1.AssertExpectations(t)
	pkg2.AssertExpectations(t)

	toRetErr := fmt.Errorf("scuffed")
	pkg3 := new(mockCpuPackage)
	pkg3.On("applyUncore").Return(toRetErr)
	topo = &cpuTopology{packages: packageList{42: pkg3}}
	assert.ErrorIs(t, topo.applyUncore(), toRetErr)
}

func TestCpuTopology_GetEffectiveUncore(t *testing.T) {
	uncore := new(mockUncore)
	topo := &cpuTopology{uncore: uncore}

	assert.Equal(t, uncore, topo.getEffectiveUncore())

	topo.uncore = nil
	assert.Equal(t, defaultUncore, topo.getEffectiveUncore())
}

func TestCpuPackage_SetUncoreFrequency(t *testing.T) {
	uncore := &uncoreFreq{}
	die := new(mockCpuDie)
	die.On("applyUncore").Return(nil)
	pkg := cpuPackage{
		dies: dieList{0: die},
	}

	assert.NoError(t, pkg.SetUncore(uncore))
	assert.Equal(t, uncore, pkg.uncore)
	die.AssertExpectations(t)
}

func TestCpuPackage_applyUncore(t *testing.T) {
	die1 := new(mockCpuDie)
	die1.On("applyUncore").Return(nil)
	die2 := new(mockCpuDie)
	die2.On("applyUncore").Return(nil)

	pkg := &cpuPackage{dies: dieList{0: die1, 1: die2}}
	assert.NoError(t, pkg.applyUncore())
	die1.AssertExpectations(t)
	die2.AssertExpectations(t)

	toRetErr := fmt.Errorf("scuffed")
	die3 := new(mockCpuDie)
	die3.On("applyUncore").Return(toRetErr)
	pkg = &cpuPackage{dies: dieList{42: die3}}
	assert.ErrorIs(t, pkg.applyUncore(), toRetErr)
}

func TestCpuPackage_getEffectiveUncore(t *testing.T) {
	topo := new(mockCpuTopology)
	uncore := new(mockUncore)
	pkg := &cpuPackage{
		topology: topo,
		uncore:   uncore,
	}
	topo.AssertNotCalled(t, "getEffectiveUncore")
	assert.Equal(t, uncore, pkg.getEffectiveUncore())

	topo = new(mockCpuTopology)
	uncore = new(mockUncore)
	topo.On("getEffectiveUncore").Return(uncore)
	pkg = &cpuPackage{topology: topo}
	assert.Equal(t, uncore, pkg.getEffectiveUncore())
	topo.AssertExpectations(t)
}

func TestCpuDie_SetUncoreFrequency(t *testing.T) {
	uncore := new(mockUncore)
	uncore.On("write", uint(1), uint(0)).Return(nil)

	pkg := new(mockCpuPackage)
	pkg.On("getID").Return(uint(1))

	die := &cpuDie{
		parentSocket: pkg,
		id:           0,
	}

	assert.NoError(t, die.SetUncore(uncore))

	assert.Equal(t, uncore, die.uncore)
	pkg.AssertExpectations(t)
	uncore.AssertExpectations(t)
}

func TestCpuDie_getEffectiveUncore(t *testing.T) {
	pkg := new(mockCpuPackage)
	uncore := new(mockUncore)
	die := &cpuDie{
		parentSocket: pkg,
		uncore:       uncore,
	}
	pkg.AssertNotCalled(t, "getEffectiveUncore")
	assert.Equal(t, uncore, die.getEffectiveUncore())

	pkg = new(mockCpuPackage)
	uncore = new(mockUncore)
	pkg.On("getEffectiveUncore").Return(uncore)
	die = &cpuDie{parentSocket: pkg}
	assert.Equal(t, uncore, die.getEffectiveUncore())
	pkg.AssertExpectations(t)
}

func TestCpuDie_applyUncore(t *testing.T) {
	uncore := new(mockUncore)
	uncore.On("write", uint(2), uint(2)).Return(nil)

	pkg := new(mockCpuPackage)
	pkg.On("getID").Return(uint(2))

	die := &cpuDie{
		parentSocket: pkg,
		id:           2,
		uncore:       uncore,
	}

	assert.NoError(t, die.applyUncore())

	pkg.AssertExpectations(t)
	uncore.AssertExpectations(t)

	//error writing
	uncore = new(mockUncore)
	expectedErr := fmt.Errorf("")
	uncore.On("write", uint(2), uint(2)).Return(expectedErr)

	pkg = new(mockCpuPackage)
	pkg.On("getID").Return(uint(2))

	die = &cpuDie{
		parentSocket: pkg,
		id:           2,
		uncore:       uncore,
	}

	assert.ErrorIs(t, die.applyUncore(), expectedErr)

	pkg.AssertExpectations(t)
	uncore.AssertExpectations(t)
}

func TestNormalizeUncoreFrequency(t *testing.T) {
	assert.Equal(t, uint(1_500_000), normalizeUncoreFreq(1_511_111))
	assert.Equal(t, uint(1_500_000), normalizeUncoreFreq(1_500_000))
	assert.Equal(t, uint(0), normalizeUncoreFreq(12))
	assert.Equal(t, uint(1_100_000), normalizeUncoreFreq(1_100_001))
}
