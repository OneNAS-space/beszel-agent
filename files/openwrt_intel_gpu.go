//go:build linux && openwrt

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	// "github.com/henrygd/beszel/agent/utils"
	"github.com/henrygd/beszel/internal/entities/system"
)

type intelGpuStats struct {
	PowerGPU float64
	PowerPkg float64
	Engines  map[string]float64
}

var (
	gpuStateMu sync.Mutex
	lastRc6 uint64
	lastEnergy uint64
	lastTime time.Time
	intelGpuStatsCmd *exec.Cmd
)

func (gm *GPUManager) updateIntelFromStats(sample *intelGpuStats) bool {
	gm.Lock()
	defer gm.Unlock()

	id := "i0"
	gpuData, ok := gm.GpuDataMap[id]
	if !ok {
		gpuData = &system.GPUData{Name: "GPU", Engines: make(map[string]float64)}
		gm.GpuDataMap[id] = gpuData
	}

	gpuData.Power += sample.PowerGPU
	gpuData.PowerPkg += sample.PowerPkg

	if gpuData.Engines == nil {
		gpuData.Engines = make(map[string]float64, len(sample.Engines))
	}
	for name, engine := range sample.Engines {
		gpuData.Engines[name] += engine
	}

	gpuData.Count++
	return true
}

func (gm *GPUManager) collectIntelStats() (err error) {
	gpuStateMu.Lock()
	defer gpuStateMu.Unlock()

	device := os.Getenv("INTEL_GPU_DEVICE")
	if device == "" {
		device = "card0"
	}
	device = filepath.Base(device)

	rc6Path := "/sys/class/drm/" + device + "/power/rc6_residency_ms"
	powerPath := "/sys/devices/virtual/powercap/intel-rapl/intel-rapl:0/intel-rapl:0:1/energy_uj"
	
	rc6Data, err := os.ReadFile(rc6Path)
	if err != nil {
		return err
	}
	currRc6, _ := strconv.ParseUint(strings.TrimSpace(string(rc6Data)), 10, 64)

	energyData, _ := os.ReadFile(powerPath)
	currEnergy, _ := strconv.ParseUint(strings.TrimSpace(string(energyData)), 10, 64)

	now := time.Now()
	
	if !lastTime.IsZero() {
		timeDelta := uint64(now.Sub(lastTime).Milliseconds())
		if timeDelta > 500 {
			rc6Delta := currRc6 - lastRc6
			usage := 100.0 - (float64(rc6Delta) / float64(timeDelta) * 100.0)
			if usage < 0 { usage = 0 }

			energyDelta := currEnergy - lastEnergy
			power := float64(energyDelta) / float64(timeDelta) / 1000.0 // Watts

			sample := intelGpuStats{
				PowerGPU: power,
				Engines: map[string]float64{
					"Render/3D": usage,
				},
			}
			gm.updateIntelFromStats(&sample)
		}
	}

	lastRc6 = currRc6
	lastEnergy = currEnergy
	lastTime = now

	return nil
}
