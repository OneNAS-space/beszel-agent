//go:build linux && openwrt

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/henrygd/beszel/agent/utils"
	"github.com/henrygd/beszel/internal/entities/systemd"
)

var errNoActiveTime = errors.New("no active time")

type systemdManager struct {
	sync.Mutex
	serviceStatsMap map[string]*systemd.Service
	isRunning       bool
	hasFreshStats   bool
	patterns        []string
}

func isSystemdAvailable() bool {
	return true
}

func newSystemdManager() (*systemdManager, error) {
	if skipSystemd, _ := utils.GetEnv("SKIP_SYSTEMD"); skipSystemd == "true" {
		return nil, nil
	}

	manager := &systemdManager{
		serviceStatsMap: make(map[string]*systemd.Service),
		patterns:        getServicePatterns(),
	}

	manager.startWorker(nil)

	return manager, nil
}

func (sm *systemdManager) startWorker(conn *dbus.Conn) {
	if sm.isRunning {
		return
	}
	sm.isRunning = true
	_ = sm.getServiceStats(nil, true)
	go func() {
		for {
			time.Sleep(time.Minute * 10)
			_ = sm.getServiceStats(nil, true)
		}
	}()
}

func (sm *systemdManager) getServiceStatsCount() int {
	return len(sm.serviceStatsMap)
}

func (sm *systemdManager) getFailedServiceCount() uint16 {
	return 0 
}

func (sm *systemdManager) getServiceStats(conn *dbus.Conn, refresh bool) []*systemd.Service {
	var services []*systemd.Service

	if !refresh {
		sm.Lock()
		defer sm.Unlock()
		for _, service := range sm.serviceStatsMap {
			services = append(services, service)
		}
		sm.hasFreshStats = false
		return services
	}

	out, err := exec.Command("ubus", "call", "service", "list").Output()
	if err != nil {
		return nil
	}

	type UbusInstance struct {
		Running bool `json:"running"`
		Pid     int  `json:"pid"`
	}
	type UbusService struct {
		Instances map[string]UbusInstance `json:"instances"`
	}

	var ubusData map[string]UbusService
	if err := json.Unmarshal(out, &ubusData); err != nil {
		return nil
	}

	currentUnits := make(map[string]struct{})
	pageSize := uint64(os.Getpagesize())
	if pageSize == 0 {
		pageSize = 4096
	}

	sm.Lock()
	defer sm.Unlock()

	for sName, sData := range ubusData {
		unitName := sName + ".service"
		currentUnits[unitName] = struct{}{}

		pid := 0
		isRunning := false
		for _, inst := range sData.Instances {
			if inst.Running && inst.Pid > 0 {
				pid = inst.Pid
				isRunning = true
				break
			}
		}

		service, exists := sm.serviceStatsMap[unitName]
		if !exists {
			service = &systemd.Service{Name: sName}
			sm.serviceStatsMap[unitName] = service
		}

		if isRunning {
			service.State = systemd.ParseServiceStatus("active")
			service.Sub = systemd.ParseServiceSubState("running")

			if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
				binEnd := strings.LastIndex(string(data), ")")
				if binEnd != -1 && len(string(data)) > binEnd+2 {
					fields := strings.Fields(string(data)[binEnd+2:])
					if len(fields) > 21 {
						utime, _ := strconv.ParseUint(fields[11], 10, 64)
						stime, _ := strconv.ParseUint(fields[12], 10, 64)
						rss, _ := strconv.ParseUint(fields[21], 10, 64)

						service.Mem = rss * pageSize
						if service.Mem > service.MemPeak {
							service.MemPeak = service.Mem
						}
						service.UpdateCPUPercent((utime + stime) * 10000000)
					}
				}
			}
		} else {
			service.State = systemd.ParseServiceStatus("inactive")
			service.Sub = systemd.ParseServiceSubState("dead")
			service.Mem = 0
			service.UpdateCPUPercent(0)
		}
		services = append(services, service)
	}

	for unitName := range sm.serviceStatsMap {
		if _, exists := currentUnits[unitName]; !exists {
			delete(sm.serviceStatsMap, unitName)
		}
	}

	sm.hasFreshStats = true
	return services
}

func (sm *systemdManager) updateServiceStats(conn *dbus.Conn, unit dbus.UnitStatus) (*systemd.Service, error) {
	return nil, nil
}

// --- 核心修订区：移除幻觉代码，使用朴素判断复用缓存 ---
func (sm *systemdManager) getServiceDetails(serviceName string) (systemd.ServiceDetails, error) {
	details := make(systemd.ServiceDetails)
	
	// 基础配置
	uName := strings.TrimSuffix(serviceName, ".service")
	initScript := "/etc/init.d/" + uName

	// 1. 优先从缓存获取已有的运行时数据
	sm.Lock()
	service, exists := sm.serviceStatsMap[serviceName]
	sm.Unlock()

	// 初始化默认状态
	details["ActiveState"] = "inactive"
	details["SubState"] = "dead"
	details["MemoryCurrent"] = uint64(0)
	details["MemoryPeak"] = uint64(0)
	details["Result"] = "success" 

	if exists {
		// 🔴 修正：彻底移除臆想的 .String()，直接通过比对状态枚举来硬编码字符串
		if service.State == systemd.ParseServiceStatus("active") {
			details["ActiveState"] = "active"
			details["SubState"] = "running"
			details["Result"] = "success"
		} else {
			details["ActiveState"] = "inactive"
			details["SubState"] = "dead"
			details["Result"] = "success"
		}
		
		details["MemoryCurrent"] = service.Mem
		details["MemoryPeak"] = service.MemPeak
	}

	// 2. 补全元数据
	details["Id"] = serviceName
	details["Description"] = uName + " (OpenWrt Procd)"
	details["LoadState"] = "loaded"
	details["FragmentPath"] = initScript
	details["NRestarts"] = uint64(0)
	details["StatusText"] = ""

	// 3. 获取 Boot state (开机自启状态)
	unitFileState := "disabled"
	if info, err := os.Stat(initScript); err == nil && !info.Mode().IsDir() {
		// 依赖退出码 (Exit Code) 判断，0 即为 enabled
		if err := exec.Command(initScript, "enabled").Run(); err == nil {
			unitFileState = "enabled"
		}
	}
	details["UnitFileState"] = unitFileState

	// 4. 动态获取服务能力 (Capabilities)
	canStart, canStop, canReload := false, false, false
	if info, err := os.Stat(initScript); err == nil && !info.Mode().IsDir() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		
		out, _ := exec.CommandContext(ctx, initScript).CombinedOutput()
		outputStr := strings.ToLower(string(out))
		
		if strings.Contains(outputStr, "start") { canStart = true }
		if strings.Contains(outputStr, "stop") { canStop = true }
		if strings.Contains(outputStr, "reload") { canReload = true }
	}
	details["CanStart"] = canStart
	details["CanStop"] = canStop
	details["CanReload"] = canReload

	return details, nil
}

func unescapeServiceName(name string) string {
	return name
}

func getServicePatterns() []string {
	patterns := []string{}
	if envPatterns, _ := utils.GetEnv("SERVICE_PATTERNS"); envPatterns != "" {
		for pattern := range strings.SplitSeq(envPatterns, ",") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if !strings.HasSuffix(pattern, "timer") && !strings.HasSuffix(pattern, ".service") {
				pattern += ".service"
			}
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		patterns = []string{"*.service"}
	}
	return patterns
}
