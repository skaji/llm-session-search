package search

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const daemonDisclaimEnv = "_LLM_SESSION_SEARCH_DISCLAIMED"

// disclaimDaemon runs before go-daemon consumes its initialization pipe. SETEXEC
// preserves the PID, inherited descriptors, and PID-file lock across re-exec.
func disclaimDaemon() error {
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return fmt.Errorf("read macOS version: %w", err)
	}
	enabled, err := needsDaemonDisclaim(version)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if os.Getenv(daemonDisclaimEnv) == "1" {
		return os.Unsetenv(daemonDisclaimEnv)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	env := append(os.Environ(), daemonDisclaimEnv+"=1")
	return execDisclaimed(executable, os.Args, env)
}

func needsDaemonDisclaim(version string) (bool, error) {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n < 1 {
		return false, fmt.Errorf("invalid macOS version %q", version)
	}
	return n >= 27, nil
}

// spawnBlock is one size/pointer pair in Darwin's _posix_spawn_args_desc.
// See https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/spawn_internal.h.
// These private layouts and Quarantine attributes were verified on macOS 27
// arm64. Keep this implementation isolated from other operating systems.
type spawnBlock struct {
	size uint64
	data unsafe.Pointer
}

type spawnPolicyExtension struct {
	allocated int32
	count     int32
	name      [128]byte
	data      unsafe.Pointer
	size      uint64
}

func execDisclaimed(executable string, args, env []string) error {
	// The kernel follows nested pointers. Pin all backing allocations, including
	// strings and pointer arrays, until the syscall returns (only on failure).
	var pins runtime.Pinner
	defer pins.Unpin()
	cstring := func(s string) (*byte, error) {
		p, err := syscall.BytePtrFromString(s)
		if err == nil {
			pins.Pin(p)
		}
		return p, err
	}
	vector := func(values []string) ([]*byte, error) {
		out := make([]*byte, len(values)+1)
		for i, value := range values {
			p, err := cstring(value)
			if err != nil {
				return nil, err
			}
			out[i] = p
		}
		pins.Pin(&out[0])
		return out, nil
	}
	path, err := cstring(executable)
	if err != nil {
		return err
	}
	argv, err := vector(args)
	if err != nil {
		return err
	}
	envp, err := vector(env)
	if err != nil {
		return err
	}

	// _posix_spawnattr defaults from posix_spawnattr_init(), with SETEXEC.
	attr := new([256]byte)
	binary.LittleEndian.PutUint16(attr[:2], 0x0040)
	binary.LittleEndian.PutUint64(attr[56:64], 1) // psa_reserved
	for _, offset := range []int{68, 72, 76, 112, 116, 120, 124} {
		binary.LittleEndian.PutUint32(attr[offset:offset+4], ^uint32(0))
	}
	pins.Pin(attr)
	// Equivalent to responsibility_spawnattrs_setdisclaim(&attr, true):
	// version, payload size, disclaim flag, and zeroed remaining fields.
	quarantine := &[6]uint32{1, 24, 1, 0, 0, 0}
	pins.Pin(quarantine)
	extension := &spawnPolicyExtension{allocated: 1, count: 1, data: unsafe.Pointer(quarantine), size: 24}
	copy(extension.name[:], "Quarantine")
	pins.Pin(extension)
	desc := &[9]spawnBlock{
		{size: uint64(unsafe.Sizeof(*attr)), data: unsafe.Pointer(attr)},
		{},
		{},
		{size: uint64(unsafe.Sizeof(*extension)), data: unsafe.Pointer(extension)},
	}
	pins.Pin(desc)
	var pid int32
	// SYS_POSIX_SPAWN takes pid, path, descriptor, argv, envp. Success replaces
	// this process; no extra child or external helper is created.
	_, _, errno := syscall.Syscall6(syscall.SYS_POSIX_SPAWN,
		uintptr(unsafe.Pointer(&pid)), uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&envp[0])), 0)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(envp)
	if errno != 0 {
		return fmt.Errorf("disclaim daemon responsibility: %w", errno)
	}
	return fmt.Errorf("disclaim daemon responsibility: posix_spawn unexpectedly returned")
}
