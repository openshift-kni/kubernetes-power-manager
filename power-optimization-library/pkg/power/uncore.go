package power

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
)

const (
	intelUncoreKmodName = "intel_uncore_frequency"
	intelUncoreDirName  = "intel_uncore_frequency"

	intelUncorePathFmt         = intelUncoreDirName + "/package_%02d_die_%02d"
	intelUncoreInitMaxFreqFile = "initial_max_freq_khz"
	intelUncoreInitMinFreqFile = "initial_min_freq_khz"
	intelUncoreMaxFreqFile     = "max_freq_khz"
	intelUncoreMinFreqFile     = "min_freq_khz"
)

type (
	uncoreFreq struct {
		min uint
		max uint
	}
	Uncore interface {
		write(pkgID, dieID uint) error
	}
)

func NewUncore(minFreq uint, maxFreq uint) (Uncore, error) {
	if !featureList.isFeatureIdSupported(UncoreFeature) {
		return nil, featureList.getFeatureIdError(UncoreFeature)
	}

	var label string
	switch cpuIdentity.vendorID {
	case vendorIDIntel:
		label = "uncore frequency (kHz)"
	case vendorIDAMD:
		label = "DF P-state"
	default:
		return nil, fmt.Errorf("unsupported vendor: %s", cpuIdentity.vendorID)
	}

	if minFreq < defaultUncore.min {
		return nil, fmt.Errorf("requested min %s %d is lower than %d allowed by the hardware", label, minFreq, defaultUncore.min)
	}
	if maxFreq > defaultUncore.max {
		return nil, fmt.Errorf("requested max %s %d is higher than %d allowed by the hardware", label, maxFreq, defaultUncore.max)
	}
	if maxFreq < minFreq {
		return nil, fmt.Errorf("requested max %s %d cannot be lower than min %s %d", label, maxFreq, label, minFreq)
	}

	if cpuIdentity.vendorID == vendorIDIntel {
		normalizedMin := normalizeUncoreFreq(minFreq)
		normalizedMax := normalizeUncoreFreq(maxFreq)
		if normalizedMin != minFreq {
			log.Info("requested min %s %d was normalized due to driver requirements", label, minFreq, "normalized", normalizedMin)
		}
		if normalizedMax != maxFreq {
			log.Info("requested max %s %d was normalized due to driver requirements", label, maxFreq, "normalized", normalizedMax)
		}
		return &uncoreFreq{min: normalizedMin, max: normalizedMax}, nil
	}
	return &uncoreFreq{min: minFreq, max: maxFreq}, nil
}

func (u *uncoreFreq) write(pkgId, dieId uint) error {
	if !featureList.isFeatureIdSupported(UncoreFeature) {
		return nil
	}

	switch cpuIdentity.vendorID {
	case vendorIDIntel:
		return u.writeIntel(pkgId, dieId)
	case vendorIDAMD:
		return u.writeAMD(pkgId)
	default:
		return fmt.Errorf("unsupported vendor: %s", cpuIdentity.vendorID)
	}
}

var (
	// default values are populated during initialization of the feature
	defaultUncore         = &uncoreFreq{}
	kernelModulesFilePath = "/proc/modules"
)

func initUncore() featureStatus {
	feature := featureStatus{
		name:     "Uncore frequency",
		driver:   "N/A",
		initFunc: initUncore,
	}

	switch cpuIdentity.vendorID {
	case vendorIDIntel:
		err := initIntelUncore()
		if err != nil {
			feature.err = fmt.Errorf("uncore feature error: %w", err)
			return feature
		}
	case vendorIDAMD:
		err := initAMDUncore()
		if err != nil {
			feature.err = fmt.Errorf("uncore feature error: %w", err)
			return feature
		}
	default:
		feature.err = fmt.Errorf("uncore feature is not supported on %s architecture (%s vendor)",
			cpuIdentity.architecture, cpuIdentity.vendorID)
		return feature
	}

	return feature
}

func (u *uncoreFreq) writeIntel(pkgId, dieId uint) error {
	if err := os.WriteFile(
		path.Join(basePath, fmt.Sprintf(intelUncorePathFmt, pkgId, dieId), intelUncoreMaxFreqFile),
		[]byte(fmt.Sprint(u.max)),
		0644,
	); err != nil {
		return err
	}
	if err := os.WriteFile(
		path.Join(basePath, fmt.Sprintf(intelUncorePathFmt, pkgId, dieId), intelUncoreMinFreqFile),
		[]byte(fmt.Sprint(u.min)),
		0644,
	); err != nil {
		return err
	}
	return nil
}

func initIntelUncore() error {
	if !checkKernelModuleLoaded(intelUncoreKmodName) {
		return fmt.Errorf("kernel module %s not loaded", intelUncoreKmodName)
	}

	uncoreDirPath := path.Join(basePath, intelUncoreDirName)
	uncoreDir, err := os.OpenFile(uncoreDirPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open uncore dir: %w", err)
	}
	if _, err := uncoreDir.Readdirnames(1); err != nil {
		return fmt.Errorf("uncore dir empty or invalid: %w", err)
	}

	if value, err := readUncoreProperty(0, 0, intelUncoreInitMaxFreqFile); err != nil {
		return fmt.Errorf("failed to determine init freq: %w", err)
	} else {
		defaultUncore.max = value
	}
	if value, err := readUncoreProperty(0, 0, intelUncoreInitMinFreqFile); err != nil {
		return fmt.Errorf("failed to determine init freq: %w", err)
	} else {
		defaultUncore.min = value
	}

	return nil
}

func checkKernelModuleLoaded(module string) bool {
	modulesFile, err := os.Open(kernelModulesFilePath)
	if err != nil {
		return false
	}
	defer modulesFile.Close()

	reader := bufio.NewScanner(modulesFile)
	for reader.Scan() {
		if strings.Contains(reader.Text(), module) {
			return true
		}
	}
	return false
}

type hasUncore interface {
	SetUncore(uncore Uncore) error
	applyUncore() error
	getEffectiveUncore() Uncore
}

func (s *cpuTopology) SetUncore(uncore Uncore) error {
	s.uncore = uncore
	return s.applyUncore()
}

func (s *cpuTopology) getEffectiveUncore() Uncore {
	if s.uncore == nil {
		return defaultUncore
	}
	return s.uncore
}
func (s *cpuTopology) applyUncore() error {
	for _, pkg := range s.packages {
		if err := pkg.applyUncore(); err != nil {
			return err
		}
	}
	return nil
}
func (c *cpuPackage) SetUncore(uncore Uncore) error {
	c.uncore = uncore
	return c.applyUncore()
}

func (c *cpuPackage) applyUncore() error {
	for _, die := range c.dies {
		if err := die.applyUncore(); err != nil {
			return err
		}
	}
	return nil
}

func (c *cpuPackage) getEffectiveUncore() Uncore {
	if c.uncore != nil {
		return c.uncore
	}
	return c.topology.getEffectiveUncore()
}

func (d *cpuDie) SetUncore(uncore Uncore) error {
	d.uncore = uncore
	return d.applyUncore()
}

func (d *cpuDie) applyUncore() error {
	return d.getEffectiveUncore().write(d.parentSocket.getID(), d.id)
}

func (d *cpuDie) getEffectiveUncore() Uncore {
	if d.uncore != nil {
		return d.uncore
	}
	return d.parentSocket.getEffectiveUncore()
}

func readUncoreProperty(pkgID, dieID uint, property string) (uint, error) {
	fullPath := path.Join(basePath, fmt.Sprintf(intelUncorePathFmt, pkgID, dieID), property)
	return readUintFromFile(fullPath)
}

func normalizeUncoreFreq(freq uint) uint {
	return freq - (freq % uint(100_000))
}
