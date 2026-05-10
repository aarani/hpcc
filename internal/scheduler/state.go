package scheduler

import (
	"time"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

type WorkerState struct {
	WorkerID        string
	PublicAddr      string
	ImageDigests    []string
	AvailableVCPUs  int32
	CurrentLoad     int32
	Runtime         gen.RuntimeType
	CertFingerprint []byte
	ActiveVMs       []VMInfo
	LastHeartbeat   time.Time
}

type VMInfo struct {
	VMID        string
	TenantID    string
	ImageDigest string
	State       gen.VMState
}

func workerStateFromRegistration(in *gen.RegisterWorkerRequest) *WorkerState {
	return &WorkerState{
		WorkerID:        in.WorkerId,
		PublicAddr:      in.PublicAddr,
		ImageDigests:    in.ImageDigests,
		AvailableVCPUs:  in.AvailableVcpus,
		CurrentLoad:     in.CurrentLoad,
		Runtime:         in.Runtime,
		CertFingerprint: in.CertFingerprint,
		LastHeartbeat:   time.Now(),
	}
}

func (w *WorkerState) applyHeartbeat(in *gen.WorkerHeartbeat) {
	w.AvailableVCPUs = in.AvailableVcpus
	w.CurrentLoad = in.CurrentLoad
	w.LastHeartbeat = time.Now()

	w.ActiveVMs = make([]VMInfo, len(in.ActiveVms))
	for i, vm := range in.ActiveVms {
		w.ActiveVMs[i] = VMInfo{
			VMID:        vm.VmId,
			TenantID:    vm.TenantId,
			ImageDigest: vm.ImageDigest,
			State:       vm.State,
		}
	}
}
