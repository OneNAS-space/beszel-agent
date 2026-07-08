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

// getProcdServiceMetrics 动态获取 procd 服务的真实 PID 和并发 Task 数量
func getProcdServiceMetrics(uName string) (uint64, uint64) {
	// 1. 定向查询：绝不拉取全局数据，将系统开销降到最低 (取自新版)
	cmd := exec.Command("ubus", "call", "service", "list", fmt.Sprintf(`{"name":"%s"}`, uName))
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}

	// 2. 强类型解析：告别脆弱的类型断言，安全且优雅 (取自旧版)
	type UbusInstance struct {
		Running bool `json:"running"`
		Pid     int  `json:"pid"`
	}
	type UbusService struct {
		Instances map[string]UbusInstance `json:"instances"`
	}
	
	var ubusData map[string]UbusService
	if err := json.Unmarshal(out, &ubusData); err != nil {
		return 0, 0
	}

	// 提取 Main PID
	var mainPid int
	if sData, ok := ubusData[uName]; ok {
		for _, inst := range sData.Instances {
			if inst.Running && inst.Pid > 0 {
				mainPid = inst.Pid
				break
			}
		}
	}

	if mainPid == 0 {
		return 0, 0 // 服务未运行
	}

	// 3. 计算并发 Tasks：直接读取 task 目录项数量，避免字符串切割开销 (取自新版)
	var taskCount uint64 = 1
	taskDir := fmt.Sprintf("/proc/%d/task", mainPid)
	if entries, err := os.ReadDir(taskDir); err == nil {
		taskCount = uint64(len(entries))
	}

	return uint64(mainPid), taskCount
}

// --- 核心修订区：纯净、专业的详情获取逻辑 ---
func (sm *systemdManager) getServiceDetails(serviceName string) (systemd.ServiceDetails, error) {
	details := make(systemd.ServiceDetails)
	
	// 基础配置
	uName := strings.TrimSuffix(serviceName, ".service")
	initScript := "/etc/init.d/" + uName

	// 1. 优先从缓存获取已有的运行状态 (CPU/MEM)
	sm.Lock()
	service, exists := sm.serviceStatsMap[serviceName]
	if !exists {
		service, exists = sm.serviceStatsMap[uName]
	}
	sm.Unlock()

	// 2. 动态获取真实的 PID 和 Tasks
	realPid, realTasks := getProcdServiceMetrics(uName)

	// 3. 严谨的状态赋值
	if exists && service.State == systemd.ParseServiceStatus("active") {
		details["ActiveState"] = "active"
		details["SubState"] = "running"
		details["Result"] = "success"
		details["MemoryCurrent"] = service.Mem
		details["MemoryPeak"] = service.MemPeak
	} else {
		details["ActiveState"] = "inactive"
		details["SubState"] = "dead"
		details["Result"] = "success"
		details["MemoryCurrent"] = uint64(0)
		details["MemoryPeak"] = uint64(0)
	}

	// 填入真实的系统数据
	details["MainPID"] = realPid
	details["TasksCurrent"] = realTasks

	// 4. 补全元数据
	details["Id"] = serviceName
	details["Description"] = uName + " (OpenWrt Procd)"
	details["LoadState"] = "loaded"
	details["FragmentPath"] = initScript
	details["NRestarts"] = uint64(0)
	details["StatusText"] = ""

	// 5. 获取 Boot state (开机自启状态)
	unitFileState := "disabled"
	if info, err := os.Stat(initScript); err == nil && !info.Mode().IsDir() {
		if err := exec.Command(initScript, "enabled").Run(); err == nil {
			unitFileState = "enabled"
		}
	}
	details["UnitFileState"] = unitFileState

	// 6. 动态获取服务能力 (Capabilities)
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
