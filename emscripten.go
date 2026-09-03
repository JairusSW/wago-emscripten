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

var processStarted = time.Now()

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
	errno := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		code := int32(-emscriptenENOSYS)
		results[0] = uint64(uint32(code))
	})
	zero := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 0 })
	dateNow := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		results[0] = math.Float64bits(float64(time.Now().UnixNano()) / float64(time.Millisecond))
	})
	monotonicNow := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		results[0] = math.Float64bits(float64(time.Since(processStarted).Nanoseconds()) / float64(time.Millisecond))
	})
	bindings := []hostBinding{
		{"abort", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: abort")})
		}, nil, nil, "terminate after Emscripten abort"},
		{"strftime", zero, i32s(4), []wago.ValType{i32}, "report unsupported locale formatting"},
		{"system", errno, i32s(1), []wago.ValType{i32}, "reject subprocess execution"},
		{"exit", func(_ wago.HostModule, params, _ []uint64) { panic(wago.HostExit{Code: int32(uint32(params[0]))}) }, i32s(1), nil, "terminate with the requested exit status"},
		{"emscripten_date_now", dateNow, nil, []wago.ValType{f64}, "return wall-clock milliseconds"},
		{"emscripten_get_now", monotonicNow, nil, []wago.ValType{f64}, "return monotonic milliseconds"},
		{"emscripten_get_now_res", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = math.Float64bits(0.001) }, nil, []wago.ValType{f64}, "report monotonic clock resolution in milliseconds"},
		{"_emscripten_get_now_is_monotonic", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 1 }, nil, []wago.ValType{i32}, "report a monotonic clock"},
		{"_mktime_js", p.mktime, i32s(1), []wago.ValType{i64}, "convert a UTC broken-down time to Unix seconds"},
		{"_localtime_js", p.localtime32, i32s(2), nil, "write deterministic UTC broken-down time"},
		{"__wago_localtime_js_i64", p.time64, []wago.ValType{i64, i32}, nil, "write deterministic UTC broken-down time"},
		{"__wago_gmtime_js_i64", p.time64, []wago.ValType{i64, i32}, nil, "write UTC broken-down time"},
		{"_tzset_js", p.tzset, i32s(3), nil, "write UTC timezone metadata"},
		{"__wago_tzset_js_4", p.tzset, i32s(4), nil, "write UTC timezone metadata"},
		{"emscripten_resize_heap", p.resizeHeap, i32s(1), []wago.ValType{i32}, "grow the guest heap up to its declared maximum"},
		{"emscripten_get_heap_max", func(module wago.HostModule, _ []uint64, results []uint64) {
			results[0] = uint64(uint32(len(module.Memory())))
		}, nil, []wago.ValType{i32}, "return the currently addressable heap size"},
		{"emscripten_notify_memory_growth", func(wago.HostModule, []uint64, []uint64) {}, i32s(1), nil, "acknowledge guest memory growth"},
		{"emscripten_memcpy_big", p.memcpyBig, i32s(3), []wago.ValType{i32}, "copy a large overlapping-safe memory range"},
		{"_abort_js", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: abort")})
		}, nil, nil, "terminate after Emscripten abort"},
		{"_emscripten_throw_longjmp", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: longjmp crossed the host boundary")})
		}, nil, nil, "trap unsupported JavaScript longjmp"},
	}
	for _, signature := range []string{
		"v", "vi", "vii", "viii", "viiii", "viiiii", "vj", "vij", "viji",
		"i", "ii", "iii", "iiii", "iiiii", "ij", "iji", "j", "ji", "jii",
		"f", "fi", "fii", "d", "di", "dii",
	} {
		params, results, ok := invokeSignature(signature)
		if !ok {
			continue
		}
		name := "invoke_" + signature
		bindings = append(bindings, hostBinding{name, p.invoke(name), params, results, "re-enter the active guest through a typed indirect callback"})
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
		hostBinding{"__wago_munmap_js_i64", zero, []wago.ValType{i32, i32, i32, i32, i32, i64}, []wago.ValType{i32}, "accept release of an anonymous mapping"},
		hostBinding{"__wago_mmap_js_i64", errno, []wago.ValType{i32, i32, i32, i32, i64, i32, i32}, []wago.ValType{i32}, "reject unsupported file mappings"},
	)
	for _, binding := range bindings {
		m.Func(binding.name, binding.fn).Params(binding.params...).Results(binding.results...).Docs(binding.docs)
	}
	return nil
}

func (p *plugin) invoke(name string) wago.HostFunc {
	return func(module wago.HostModule, params, results []uint64) {
		got, err := p.invoker.Invoke(context.Background(), module, "__wago_"+name, params...)
		if err != nil {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: %s: %w", name, err)})
		}
		copy(results, got)
	}
}

func (p *plugin) tzset(module wago.HostModule, params, _ []uint64) {
	mem := module.Memory()
	if len(params) < 3 {
		return
	}
	putU32(mem, uint32(params[0]), 0)
	putU32(mem, uint32(params[1]), 0)
	putCString(mem, uint32(params[2]), "UTC+0000", 17)
	if len(params) == 4 {
		putCString(mem, uint32(params[3]), "UTC+0000", 17)
	}
}

func (p *plugin) resizeHeap(module wago.HostModule, params, results []uint64) {
	got, err := p.invoker.Invoke(context.Background(), module, "__wago_resize_heap", params...)
	if err != nil || len(got) != 1 {
		if err != nil {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: resize heap: %w", err)})
		}
		results[0] = 0
		return
	}
	results[0] = got[0]
}

func (p *plugin) memcpyBig(module wago.HostModule, params, results []uint64) {
	dst, src, size := uint32(params[0]), uint32(params[1]), uint32(params[2])
	mem := module.Memory()
	if uint64(dst)+uint64(size) > uint64(len(mem)) || uint64(src)+uint64(size) > uint64(len(mem)) {
		panic(wago.HostTrap{Err: fmt.Errorf("emscripten: memcpy range exceeds memory")})
	}
	copy(mem[dst:dst+size], mem[src:src+size])
	results[0] = uint64(dst)
}

func (p *plugin) time64(module wago.HostModule, params, _ []uint64) {
	writeTM(module.Memory(), uint32(params[1]), time.Unix(int64(params[0]), 0).UTC())
}

func (p *plugin) localtime32(module wago.HostModule, params, _ []uint64) {
	writeTM(module.Memory(), uint32(params[1]), time.Unix(int64(int32(params[0])), 0).UTC())
}

func (p *plugin) mktime(module wago.HostModule, params, results []uint64) {
	mem, ptr := module.Memory(), uint32(params[0])
	if uint64(ptr)+36 > uint64(len(mem)) {
		results[0] = ^uint64(0)
		return
	}
	read := func(offset uint32) int { return int(int32(binary.LittleEndian.Uint32(mem[ptr+offset : ptr+offset+4]))) }
	tm := time.Date(read(20)+1900, time.Month(read(16)+1), read(12), read(8), read(4), read(0), 0, time.UTC)
	writeTM(mem, ptr, tm)
	results[0] = uint64(tm.Unix())
}

func writeTM(mem []byte, ptr uint32, tm time.Time) {
	if uint64(ptr)+36 > uint64(len(mem)) {
		return
	}
	yday := tm.YearDay() - 1
	values := []int32{int32(tm.Second()), int32(tm.Minute()), int32(tm.Hour()), int32(tm.Day()), int32(tm.Month() - 1), int32(tm.Year() - 1900), int32(tm.Weekday()), int32(yday), 0}
	for i, value := range values {
		binary.LittleEndian.PutUint32(mem[int(ptr)+i*4:int(ptr)+i*4+4], uint32(value))
	}
}

func putU32(mem []byte, ptr, value uint32) {
	if uint64(ptr)+4 <= uint64(len(mem)) {
		binary.LittleEndian.PutUint32(mem[ptr:ptr+4], value)
	}
}

func putCString(mem []byte, ptr uint32, value string, limit uint32) {
	if limit == 0 || uint64(ptr)+uint64(limit) > uint64(len(mem)) {
		return
	}
	n := len(value)
	if n >= int(limit) {
		n = int(limit) - 1
	}
	copy(mem[ptr:ptr+uint32(n)], value[:n])
	mem[ptr+uint32(n)] = 0
}

func invokeSignature(signature string) ([]wago.ValType, []wago.ValType, bool) {
	if len(signature) == 0 {
		return nil, nil, false
	}
	valueType := func(code byte) (wago.ValType, bool) {
		switch code {
		case 'i', 'p':
			return wago.ValI32, true
		case 'j':
			return wago.ValI64, true
		case 'f':
			return wago.ValF32, true
		case 'd':
			return wago.ValF64, true
		default:
			return 0, false
		}
	}
	params := []wago.ValType{wago.ValI32}
	for i := 1; i < len(signature); i++ {
		typ, ok := valueType(signature[i])
		if !ok {
			return nil, nil, false
		}
		params = append(params, typ)
	}
	if signature[0] == 'v' {
		return params, nil, true
	}
	result, ok := valueType(signature[0])
	if !ok {
		return nil, nil, false
	}
	return params, []wago.ValType{result}, true
}
