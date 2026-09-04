package emscripten

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/wago-org/wago"
)

const emscriptenENOSYS = 52

var errEmscriptenLongjmp = errors.New("emscripten longjmp")

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
		{"setTempRet0", p.setTempRet0, i32s(1), nil, "store the high half of a legalized i64 result"},
		{"getTempRet0", p.getTempRet0, nil, []wago.ValType{i32}, "load the high half of a legalized i64 result"},
		{"emscripten_num_logical_cores", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 1 }, nil, []wago.ValType{i32}, "report one standalone execution core"},
		{"emscripten_has_threading_support", zero, nil, []wago.ValType{i32}, "report that pthreads are unavailable"},
		{"emscripten_is_main_runtime_thread", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 1 }, nil, []wago.ValType{i32}, "report execution on the main runtime thread"},
		{"emscripten_is_main_browser_thread", func(_ wago.HostModule, _ []uint64, results []uint64) { results[0] = 1 }, nil, []wago.ValType{i32}, "report the standalone main thread"},
		{"emscripten_console_log", p.console(p.stdout), i32s(1), nil, "write a UTF-8 message to stdout"},
		{"emscripten_console_warn", p.console(p.stderr), i32s(1), nil, "write a UTF-8 warning to stderr"},
		{"emscripten_console_error", p.console(p.stderr), i32s(1), nil, "write a UTF-8 error to stderr"},
		{"_abort_js", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: abort")})
		}, nil, nil, "terminate after Emscripten abort"},
		{"_emscripten_throw_longjmp", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: errEmscriptenLongjmp})
		}, nil, nil, "unwind an Emscripten-mode longjmp through an invoke trampoline"},
		{"__syscall_openat", p.syscallOpenat, i32s(4), []wago.ValType{i32}, "open a bounded in-memory file"},
		{"__syscall_fcntl64", p.syscallFcntl, i32s(3), []wago.ValType{i32}, "manage an in-memory file descriptor"},
		{"__syscall_dup3", p.syscallDup3, i32s(3), []wago.ValType{i32}, "duplicate an in-memory file descriptor"},
		{"__syscall_dup", p.syscallDup, i32s(1), []wago.ValType{i32}, "duplicate an in-memory file descriptor"},
		{"__syscall_ioctl", p.syscallIoctl, i32s(3), []wago.ValType{i32}, "report non-terminal in-memory descriptors"},
		{"__syscall_fchmod", p.syscallFchmod, i32s(2), []wago.ValType{i32}, "accept in-memory descriptor mode changes"},
		{"__syscall_fdatasync", p.syscallFDatasync, i32s(1), []wago.ValType{i32}, "synchronize an in-memory descriptor"},
		{"__syscall_fadvise64", p.syscallFadvise, []wago.ValType{i32, i64, i64, i32}, []wago.ValType{i32}, "accept in-memory access advice"},
		{"__syscall_chmod", p.syscallChmod, i32s(2), []wago.ValType{i32}, "accept in-memory path mode changes"},
		{"__syscall_readlinkat", p.syscallReadlinkat, i32s(4), []wago.ValType{i32}, "report that the in-memory filesystem has no symbolic links"},
		{"__syscall_faccessat", p.syscallFaccessat, i32s(4), []wago.ValType{i32}, "check an in-memory path"},
		{"__syscall_mkdirat", p.syscallMkdirat, i32s(3), []wago.ValType{i32}, "create an in-memory directory"},
		{"__syscall_unlinkat", p.syscallUnlinkat, i32s(3), []wago.ValType{i32}, "remove an in-memory file or directory"},
		{"__syscall_rmdir", func(module wago.HostModule, params, results []uint64) {
			p.syscallUnlinkat(module, []uint64{uint64(^uint32(99)), params[0], 0x200}, results)
		}, i32s(1), []wago.ValType{i32}, "remove an empty in-memory directory"},
		{"__syscall_renameat", p.syscallRenameat, i32s(4), []wago.ValType{i32}, "rename an in-memory path"},
		{"__syscall_getcwd", p.syscallGetcwd, i32s(2), []wago.ValType{i32}, "report the in-memory root directory"},
		{"__syscall_fstat64", p.syscallFstat, i32s(2), []wago.ValType{i32}, "report in-memory descriptor metadata"},
		{"__syscall_stat64", p.syscallStat, i32s(2), []wago.ValType{i32}, "report in-memory path metadata"},
		{"__syscall_lstat64", p.syscallStat, i32s(2), []wago.ValType{i32}, "report in-memory path metadata"},
		{"__syscall_newfstatat", p.syscallNewfstatat, i32s(4), []wago.ValType{i32}, "report in-memory path metadata"},
		{"__syscall_fallocate", p.syscallFallocate, []wago.ValType{i32, i32, i64, i64}, []wago.ValType{i32}, "allocate in-memory file space"},
		{"__syscall_truncate64", p.syscallTruncate, []wago.ValType{i32, i64}, []wago.ValType{i32}, "resize an in-memory path"},
		{"__syscall_utimensat", p.syscallUtimensat, i32s(4), []wago.ValType{i32}, "accept in-memory timestamp changes"},
		{"__wago_wasi_fd_write", p.wasiFDWrite, i32s(4), []wago.ValType{i32}, "write stdio or an in-memory file"},
		{"__wago_wasi_fd_read", p.wasiFDRead, i32s(4), []wago.ValType{i32}, "read stdin or an in-memory file"},
		{"__wago_wasi_fd_close", p.wasiFDClose, i32s(1), []wago.ValType{i32}, "close an in-memory file descriptor"},
		{"__wago_wasi_fd_seek", p.wasiFDSeek, []wago.ValType{i32, i64, i32, i32}, []wago.ValType{i32}, "seek an in-memory file descriptor"},
		{"__wago_wasi_fd_fdstat_get", p.wasiFDStatGet, i32s(2), []wago.ValType{i32}, "report descriptor type and rights"},
		{"__wago_wasi_fd_filestat_get", p.wasiFileStatGet, i32s(2), []wago.ValType{i32}, "report descriptor metadata"},
		{"__wago_wasi_fd_sync", p.wasiFDSync, i32s(1), []wago.ValType{i32}, "synchronize an in-memory descriptor"},
		{"__wago_wasi_fd_datasync", p.wasiFDSync, i32s(1), []wago.ValType{i32}, "synchronize an in-memory descriptor"},
		{"__wago_wasi_fd_fdstat_set_flags", p.wasiFDSetFlags, i32s(2), []wago.ValType{i32}, "set in-memory descriptor flags"},
		{"__wago_wasi_fd_filestat_set_size", p.wasiFDSetSize, []wago.ValType{i32, i64}, []wago.ValType{i32}, "resize an in-memory file"},
		{"__wago_wasi_fd_pread", p.wasiPositionedIO(false), []wago.ValType{i32, i32, i32, i64, i32}, []wago.ValType{i32}, "read an in-memory file at an offset"},
		{"__wago_wasi_fd_pwrite", p.wasiPositionedIO(true), []wago.ValType{i32, i32, i32, i64, i32}, []wago.ValType{i32}, "write an in-memory file at an offset"},
		{"__wago_wasi_fd_advise", p.wasiFDAdvise, []wago.ValType{i32, i64, i64, i32}, []wago.ValType{i32}, "accept in-memory access advice"},
		{"__wago_wasi_fd_allocate", p.wasiFDAllocate, []wago.ValType{i32, i64, i64}, []wago.ValType{i32}, "allocate in-memory file space"},
		{"__wago_wasi_environ_sizes_get", p.wasiEnvironSizesGet, i32s(2), []wago.ValType{i32}, "report the configured standalone environment size"},
		{"__wago_wasi_environ_get", p.wasiEnvironGet, i32s(2), []wago.ValType{i32}, "write the configured standalone environment"},
	}
	for _, signature := range invokeSignatures() {
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
		{"__syscall_chdir", 1}, {"__syscall_fchdir", 1}, {"__syscall_fchmodat2", 4},
		{"__syscall_fchown32", 3}, {"__syscall_fchownat", 5},
		{"__syscall_fstatfs64", 3}, {"__syscall_statfs64", 3}, {"__syscall_getdents64", 3},
		{"__syscall_mknodat", 4}, {"__syscall_pipe", 1}, {"__syscall_poll", 3}, {"__syscall_symlinkat", 3},
		{"__syscall__newselect", 5}, {"__syscall_accept4", 6}, {"__syscall_bind", 6}, {"__syscall_connect", 6},
		{"__syscall_getpeername", 6}, {"__syscall_getsockname", 6}, {"__syscall_getsockopt", 6},
		{"__syscall_listen", 6}, {"__syscall_recvfrom", 6}, {"__syscall_recvmsg", 6},
		{"__syscall_sendmsg", 6}, {"__syscall_sendto", 6}, {"__syscall_shutdown", 6}, {"__syscall_socket", 6},
	} {
		bindings = append(bindings, hostBinding{nameAndArity.name, errno, i32s(nameAndArity.arity), []wago.ValType{i32}, "fail closed with ENOSYS"})
	}
	bindings = append(bindings,
		hostBinding{"__syscall_ftruncate64", p.syscallFtruncate, []wago.ValType{i32, i64}, []wago.ValType{i32}, "resize an in-memory file"},
		hostBinding{"_munmap_js", p.munmap, i32s(6), []wago.ValType{i32}, "synchronize release of an in-memory file mapping"},
		hostBinding{"_mmap_js", p.mmap32, i32s(7), []wago.ValType{i32}, "map an in-memory file into guest memory"},
		hostBinding{"__wago_munmap_js_i64", p.munmap, []wago.ValType{i32, i32, i32, i32, i32, i64}, []wago.ValType{i32}, "synchronize release of an in-memory file mapping"},
		hostBinding{"__wago_mmap_js_i64", p.mmap64, []wago.ValType{i32, i32, i32, i32, i64, i32, i32}, []wago.ValType{i32}, "map an in-memory file into guest memory"},
	)
	for _, binding := range bindings {
		m.Func(binding.name, binding.fn).Params(binding.params...).Results(binding.results...).Docs(binding.docs)
	}
	return nil
}

func invokeSignatures() []string {
	seen := make(map[string]bool)
	var signatures []string
	add := func(signature string) {
		if !seen[signature] {
			seen[signature] = true
			signatures = append(signatures, signature)
		}
	}
	for params := 0; params <= 16; params++ {
		for _, result := range []byte{'v', 'i', 'j'} {
			add(string(result) + strings.Repeat("i", params))
		}
	}
	for _, signature := range []string{
		"f", "fi", "fii", "fiii", "fiiii", "d", "dd", "ddd", "di", "dii", "diii", "diiii", "dj",
		"id", "idd", "idi", "idii", "idiii", "idiiii", "if", "iff", "ifi", "ifii", "iid", "iidi", "iidii", "iif", "iifii", "ij", "iji", "iij", "iiji", "iijii",
		"vd", "vid", "vidi", "vidii", "vif", "vifi", "vifii", "viid", "vj", "vij", "viji", "vijii", "vjj", "vji", "vjii",
		"jd", "jf", "jj", "jji", "jij", "jiji",
	} {
		add(signature)
	}
	return signatures
}

func supportsInvoke(name string) bool {
	if !strings.HasPrefix(name, "invoke_") {
		return false
	}
	wanted := strings.TrimPrefix(name, "invoke_")
	for _, signature := range invokeSignatures() {
		if signature == wanted {
			return true
		}
	}
	return false
}

func (p *plugin) invoke(name string) wago.HostFunc {
	return func(module wago.HostModule, params, results []uint64) {
		stack, _ := p.invoker.Invoke(context.Background(), module, "emscripten_stack_get_current")
		got, err := p.invoker.Invoke(context.Background(), module, "__wago_"+name, params...)
		if err != nil && errors.Is(err, errEmscriptenLongjmp) {
			if len(stack) == 1 {
				if _, restoreErr := p.invoker.Invoke(context.Background(), module, "_emscripten_stack_restore", stack[0]); restoreErr != nil {
					panic(wago.HostTrap{Err: fmt.Errorf("emscripten: %s restore after longjmp: %w", name, restoreErr)})
				}
			}
			if _, threwErr := p.invoker.Invoke(context.Background(), module, "setThrew", 1, 0); threwErr != nil {
				panic(wago.HostTrap{Err: fmt.Errorf("emscripten: %s record longjmp: %w", name, threwErr)})
			}
			clear(results)
			return
		}
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

func (p *plugin) console(writer io.Writer) wago.HostFunc {
	return func(module wago.HostModule, params, _ []uint64) {
		message, ok := cString(module.Memory(), uint32(params[0]))
		if !ok {
			panic(wago.HostTrap{Err: fmt.Errorf("emscripten: console string exceeds memory")})
		}
		_, _ = fmt.Fprintln(writer, message)
	}
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
