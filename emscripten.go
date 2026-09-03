package emscripten

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/wago-org/wago"
)

const emscriptenENOSYS = 52

type hostBinding struct {
	name    string
	fn      wago.HostFunc
	params  []wago.ValType
	results []wago.ValType
	docs    string
}

func (p *plugin) registerEmscripten(imports *wago.HostImportRegistrar) error {
	m, err := imports.Module("env")
	if err != nil {
		return err
	}
	i32, i64, f64 := wago.ValI32, wago.ValI64, wago.ValF64
	i32s := func(n int) []wago.ValType {
		out := make([]wago.ValType, n)
		for i := range out {
			out[i] = i32
		}
		return out
	}
	noResult := wago.HostFunc(func(wago.HostModule, []uint64, []uint64) {})
	errno := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		code := int32(-emscriptenENOSYS)
		results[0] = uint64(uint32(code))
	})
	zero := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 0 })
	now := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		results[0] = math.Float64bits(float64(time.Now().UnixNano()) / float64(time.Millisecond))
	})
	bindings := []hostBinding{
		{"abort", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: abort")})
		}, nil, nil, "terminate after Emscripten abort"},
		{"invoke_vii", p.invokeVII, i32s(3), nil, "re-enter the active guest through a typed indirect callback"},
		{"strftime", zero, i32s(4), []wago.ValType{i32}, "report unsupported locale formatting"},
		{"system", errno, i32s(1), []wago.ValType{i32}, "reject subprocess execution"},
		{"exit", func(_ wago.HostModule, params, _ []uint64) { panic(wago.HostExit{Code: int32(uint32(params[0]))}) }, i32s(1), nil, "terminate with the requested exit status"},
		{"emscripten_date_now", now, nil, []wago.ValType{f64}, "return wall-clock milliseconds"},
		{"emscripten_get_now", now, nil, []wago.ValType{f64}, "return wall-clock milliseconds"},
		{"_emscripten_get_now_is_monotonic", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 1 }, nil, []wago.ValType{i32}, "report a monotonic clock"},
		{"_mktime_js", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = ^uint64(0) }, i32s(1), []wago.ValType{i64}, "report unsupported host-local mktime"},
		{"_localtime_js", noResult, i32s(2), nil, "leave an unsupported local-time result empty"},
		{"__wago_localtime_js_i64", noResult, []wago.ValType{i64, i32}, nil, "leave an unsupported 64-bit local-time result empty"},
		{"__wago_gmtime_js_i64", noResult, []wago.ValType{i64, i32}, nil, "leave an unsupported 64-bit UTC result empty"},
		{"_tzset_js", p.tzset, i32s(3), nil, "write UTC timezone metadata"},
		{"emscripten_resize_heap", zero, i32s(1), []wago.ValType{i32}, "deny host-driven heap resize"},
		{"_emscripten_throw_longjmp", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: longjmp crossed the host boundary")})
		}, nil, nil, "trap unsupported JavaScript longjmp"},
	}
	for _, nameAndArity := range []struct {
		name  string
		arity int
	}{
		{"__syscall_faccessat", 4}, {"__syscall_fchmod", 2}, {"__syscall_chmod", 2},
		{"__syscall_fchown32", 3}, {"__syscall_fcntl64", 3}, {"__syscall_openat", 4},
		{"__syscall_ioctl", 3}, {"__syscall_fstat64", 2}, {"__syscall_stat64", 2},
		{"__syscall_newfstatat", 4}, {"__syscall_lstat64", 2}, {"__syscall_getcwd", 2},
		{"__syscall_mkdirat", 3}, {"__syscall_readlinkat", 4}, {"__syscall_rmdir", 1},
		{"__syscall_unlinkat", 3}, {"__syscall_utimensat", 4}, {"__syscall_dup3", 3},
		{"__syscall_renameat", 4},
	} {
		bindings = append(bindings, hostBinding{nameAndArity.name, errno, i32s(nameAndArity.arity), []wago.ValType{i32}, "fail closed with ENOSYS"})
	}
	bindings = append(bindings,
		hostBinding{"__syscall_ftruncate64", errno, []wago.ValType{i32, i64}, []wago.ValType{i32}, "fail closed with ENOSYS"},
		hostBinding{"_munmap_js", zero, i32s(6), []wago.ValType{i32}, "accept release of an anonymous mapping"},
		hostBinding{"_mmap_js", errno, i32s(7), []wago.ValType{i32}, "reject unsupported file mappings"},
	)
	for _, binding := range bindings {
		m.Func(binding.name, binding.fn).Params(binding.params...).Results(binding.results...).Docs(binding.docs)
	}
	return nil
}

func (p *plugin) invokeVII(module wago.HostModule, params, _ []uint64) {
	if _, err := p.invoker.Invoke(context.Background(), module, "__wago_invoke_vii", params...); err != nil {
		panic(wago.HostTrap{Err: fmt.Errorf("emscripten: invoke_vii: %w", err)})
	}
}

func (p *plugin) tzset(module wago.HostModule, params, _ []uint64) {
	mem := module.Memory()
	for _, address := range params {
		ptr := uint32(address)
		if uint64(ptr)+4 <= uint64(len(mem)) {
			binary.LittleEndian.PutUint32(mem[ptr:ptr+4], 0)
		}
	}
}
