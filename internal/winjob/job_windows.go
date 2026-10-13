//go:build windows
package winjob

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

type basicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct{ ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64 }

type extendedLimitInformation struct {
	Basic                 basicLimitInformation
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type Job struct{ h syscall.Handle }

func New() (*Job, error) {
	r, _, err := procCreateJobObject.Call(0, 0)
	if r == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	j := &Job{h: syscall.Handle(r)}
	info := extendedLimitInformation{}
	info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, err := procSetInformationJobObject.Call(uintptr(j.h), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 {
		j.Close()
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return j, nil
}

func (j *Job) AssignSelf() error {
	cur, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	return j.assign(cur)
}

func (j *Job) AssignPID(pid int) error {
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	return j.assign(h)
}

func (j *Job) assign(h syscall.Handle) error {
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(j.h), uintptr(h)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

func (j *Job) Close() error {
	if j.h == 0 {
		return nil
	}
	err := syscall.CloseHandle(j.h)
	j.h = 0
	return err
}
