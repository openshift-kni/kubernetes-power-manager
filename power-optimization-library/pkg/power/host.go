package power

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
)

// The hostImpl is the backing object of Host interface
type hostImpl struct {
	name           string
	architecture   string
	vendorId       string
	exclusivePools PoolList
	reservedPool   Pool
	sharedPool     Pool
	topology       Topology
	featureStates  *FeatureSet
}

// Host represents the actual machine to be managed
type Host interface {
	SetName(name string)
	GetName() string
	GetFeaturesInfo() FeatureSet

	SetArchitecture() error
	GetArchitecture() string

	SetVendorID() error
	GetVendorID() string

	GetReservedPool() Pool
	GetSharedPool() Pool

	AddExclusivePool(poolName string) (Pool, error)
	GetExclusivePool(poolName string) Pool
	GetAllExclusivePools() *PoolList

	GetAllCpus() *CpuList
	GetFreqRanges() CoreTypeList
	Topology() Topology
	// returns number of distinct core types
	NumCoreTypes() uint
}

// create a pre-populated Host object
func initHost(nodeName string) (Host, error) {

	host := &hostImpl{
		name:           nodeName,
		exclusivePools: PoolList{},
	}
	host.featureStates = &featureList

	if err := host.SetArchitecture(); err != nil {
		return nil, fmt.Errorf("failed to set host architecture: %w", err)
	}
	if err := host.SetVendorID(); err != nil {
		return nil, fmt.Errorf("failed to set host vendorId: %w", err)
	}

	// create predefined pools
	host.reservedPool = &reservedPoolType{poolImpl{
		name:  reservedPoolName,
		mutex: &sync.Mutex{},
		host:  host,
	}}
	host.sharedPool = &sharedPoolType{poolImpl{
		name:  sharedPoolName,
		cpus:  CpuList{},
		mutex: &sync.Mutex{},
		host:  host,
	}}

	topology, err := discoverTopology(host.architecture)
	if err != nil {
		log.Error(err, "failed to discover cpuTopology")
		return nil, fmt.Errorf("failed to init host: %w", err)
	}
	for _, cpu := range *topology.CPUs() {
		cpu._setPoolProperty(host.reservedPool)
	}

	log.Info("discovered cpus", "cpus", len(*topology.CPUs()))

	host.topology = topology

	// create a shallow copy of pointers, changes to underlying cpu object will reflect in both lists,
	// changes to each list will not affect the other
	host.reservedPool.(*reservedPoolType).cpus = make(CpuList, len(*topology.CPUs()))
	copy(host.reservedPool.(*reservedPoolType).cpus, *topology.CPUs())
	return host, nil
}

func (host *hostImpl) SetName(name string) {
	host.name = name
}

func (host *hostImpl) GetName() string {
	return host.name
}

func (host *hostImpl) GetReservedPool() Pool {
	return host.reservedPool
}

func (host *hostImpl) SetArchitecture() error {
	arch, err := GetFromLscpu("^Architecture:")
	if err != nil {
		return fmt.Errorf("failed to get Architecture from lscpu: %w", err)
	}
	host.architecture = arch
	return nil
}

func (host *hostImpl) GetArchitecture() string {
	return host.architecture
}

func (host *hostImpl) SetVendorID() error {
	vendorId, err := GetFromLscpu("^Vendor ID:")
	if err != nil {
		return fmt.Errorf("failed to get VendorID from lscpu: %w", err)
	}
	host.vendorId = vendorId
	return nil
}

func (host *hostImpl) GetVendorID() string {
	return host.vendorId
}

// GetFromLscpu returns the value of a certain key from the lscpu output.
var GetFromLscpu = func(regex string) (string, error) {
	regexp.MustCompile(regex)
	cmdStr := fmt.Sprintf("lscpu | grep -Ew \"%s\" | cut -d ':' -f 2", regex)
	cmd := exec.Command("bash", "-c", cmdStr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(stderr.String()) > 0 {
		return "", fmt.Errorf("failed to get lscpu info: %s", stderr.String())
	}
	re := regexp.MustCompile(`\s+`)
	finalResult := re.ReplaceAllString(string(output), "")
	return finalResult, nil
}

// returns default min/max frequency range
func (host *hostImpl) GetFreqRanges() CoreTypeList {
	return coreTypes
}

// AddExclusivePool creates new empty pool
func (host *hostImpl) AddExclusivePool(poolName string) (Pool, error) {
	if i := host.exclusivePools.IndexOfName(poolName); i >= 0 {
		return host.exclusivePools[i], fmt.Errorf("pool with name %s already exists", poolName)
	}
	var pool Pool = &exclusivePoolType{poolImpl{
		name:  poolName,
		mutex: &sync.Mutex{},
		cpus:  make([]Cpu, 0),
		host:  host,
	}}

	host.exclusivePools.add(pool)
	return pool, nil
}

// GetExclusivePool Returns a Pool object of the exclusive pool with matching name supplied
// returns nil if not found
func (host *hostImpl) GetExclusivePool(name string) Pool {
	return host.exclusivePools.ByName(name)
}

// GetSharedPool returns shared pool
func (host *hostImpl) GetSharedPool() Pool {
	return host.sharedPool
}

func (host *hostImpl) GetFeaturesInfo() FeatureSet {
	return *host.featureStates
}

func (host *hostImpl) GetAllCpus() *CpuList {
	return host.topology.CPUs()
}

func (host *hostImpl) GetAllExclusivePools() *PoolList {
	return &host.exclusivePools
}

func (host *hostImpl) NumCoreTypes() uint {
	return uint(len(coreTypes))
}

func (host *hostImpl) Topology() Topology {
	return host.topology
}
