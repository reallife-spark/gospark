//go:build cgo && linux

// Package nvml is the NVML GPU-metrics backend for gputel. It reads per-GPU
// temperature, memory, utilization, power and clocks through go-nvlib, which
// wraps go-nvml. It is imported only by the sparknet-gpu sidecar, so no core
// goSpark binary links a GPU library.
//
// The build tag keeps this file out of a CGO_ENABLED=0 build; nvml_stub.go
// covers that case, so the package always compiles.
package nvml

import (
	"context"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	nvmlapi "github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/reallife-spark/goSpark/internal/gputel"
)

func init() {
	gputel.Register("nvml", func() gputel.Collector { return collector{} })
}

type collector struct{}

// Snapshot reads every NVML device. A missing driver is reported, not fatal:
// go-nvml dlopens libnvidia-ml.so at call time, so this degrades to a typed
// reason on a host with no NVIDIA driver.
func (collector) Snapshot(context.Context) gputel.Snapshot {
	s := gputel.Snapshot{Source: "nvml", CollectedAtUnix: time.Now().Unix()}

	nvmllib := nvmlapi.New()
	if ret := nvmllib.Init(); ret != nvmlapi.SUCCESS {
		s.Reason = "nvml: init: " + nvmlapi.ErrorString(ret)
		return s
	}
	defer func() { _ = nvmllib.Shutdown() }()

	if v, ret := nvmllib.SystemGetDriverVersion(); ret == nvmlapi.SUCCESS {
		s.DriverVersion = v
	}

	devs, err := device.New(nvmllib).GetDevices()
	if err != nil {
		s.Reason = "nvml: enumerate devices: " + err.Error()
		return s
	}
	for _, dev := range devs {
		s.GPUs = append(s.GPUs, readDevice(dev))
	}
	s.Available = true
	return s
}

func readDevice(dev device.Device) gputel.GPU {
	var g gputel.GPU
	if i, ret := dev.GetIndex(); ret == nvmlapi.SUCCESS {
		g.Index = uint32(i)
	}
	if name, ret := dev.GetName(); ret == nvmlapi.SUCCESS {
		g.Name = name
	}
	if uuid, ret := dev.GetUUID(); ret == nvmlapi.SUCCESS {
		g.UUID = uuid
	}
	if t, ret := dev.GetTemperature(nvmlapi.TEMPERATURE_GPU); ret == nvmlapi.SUCCESS {
		g.TempC = t
	}
	if m, ret := dev.GetMemoryInfo(); ret == nvmlapi.SUCCESS {
		g.MemUsedBytes = m.Used
		g.MemTotalBytes = m.Total
	}
	if u, ret := dev.GetUtilizationRates(); ret == nvmlapi.SUCCESS {
		g.UtilPct = u.Gpu
		g.MemUtilPct = u.Memory
	}
	if p, ret := dev.GetPowerUsage(); ret == nvmlapi.SUCCESS {
		g.PowerMilliwatts = p
	}
	if c, ret := dev.GetClockInfo(nvmlapi.CLOCK_SM); ret == nvmlapi.SUCCESS {
		g.SMClockMHz = c
	}
	if c, ret := dev.GetClockInfo(nvmlapi.CLOCK_MEM); ret == nvmlapi.SUCCESS {
		g.MemClockMHz = c
	}
	return g
}
