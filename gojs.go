package emscripten

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wago-org/wago"
)

type jsKind byte

const (
	jsUndefined jsKind = iota
	jsNull
	jsNumber
	jsBoolean
	jsString
	jsObject
	jsArray
	jsBytes
	jsFunction
)

type jsValue struct {
	kind       jsKind
	number     float64
	truth      bool
	text       string
	object     map[string]*jsValue
	array      []*jsValue
	bytes      []byte
	call       func([]*jsValue) (*jsValue, error)
	callbackID uint32
}

type goState struct {
	mu     sync.Mutex
	values map[uint32]*jsValue
	nextID uint32
}

var nextTimeoutID atomic.Uint32

func (p *plugin) registerGoJS(imports *wago.HostImportRegistrar) error {
	m, err := imports.Module("go")
	if err != nil {
		return err
	}
	i32 := []wago.ValType{wago.ValI32}
	bindings := []hostBinding{
		{"debug", func(wago.HostModule, []uint64, []uint64) {}, i32, nil, "ignore Go runtime debug markers"},
		{"runtime.resetMemoryDataView", p.goResetMemory, i32, nil, "refresh the command-style Go memory view"},
		{"runtime.wasmExit", p.goExit, i32, nil, "terminate the Go command"},
		{"runtime.wasmWrite", p.goWrite, i32, nil, "write Go stdout or stderr"},
		{"runtime.nanotime1", p.goNanotime, i32, nil, "return monotonic nanoseconds"},
		{"runtime.walltime", p.goWalltime, i32, nil, "return wall-clock seconds and nanoseconds"},
		{"runtime.scheduleTimeoutEvent", p.goScheduleTimeout, i32, nil, "allocate a timeout identifier without a browser event loop"},
		{"runtime.clearTimeoutEvent", func(wago.HostModule, []uint64, []uint64) {}, i32, nil, "clear a bare timeout identifier"},
		{"runtime.getRandomData", p.goRandom, i32, nil, "fill a Go byte slice with cryptographic randomness"},
		{"syscall/js.finalizeRef", func(wago.HostModule, []uint64, []uint64) {}, i32, nil, "release a bare JS reference"},
		{"syscall/js.stringVal", p.goStringVal, i32, nil, "store a Go string in the bounded JS value table"},
		{"syscall/js.valueGet", p.goValueGet, i32, nil, "read a property from the bounded JS object model"},
		{"syscall/js.valueSet", p.goValueSet, i32, nil, "write a property in the bounded JS object model"},
		{"syscall/js.valueIndex", p.goValueIndex, i32, nil, "read an indexed byte or string value"},
		{"syscall/js.valueSetIndex", p.goValueSetIndex, i32, nil, "write an indexed byte or array value"},
		{"syscall/js.valueCall", p.goValueCall, i32, nil, "invoke a bounded host method"},
		{"syscall/js.valueNew", p.goValueNew, i32, nil, "construct a bounded Uint8Array"},
		{"syscall/js.valueLength", p.goValueLength, i32, nil, "read a bounded string or byte-array length"},
		{"syscall/js.valuePrepareString", p.goPrepareString, i32, nil, "prepare an empty string representation"},
		{"syscall/js.valueLoadString", p.goLoadString, i32, nil, "copy a prepared UTF-8 string"},
		{"syscall/js.copyBytesToGo", p.goCopyBytesToGo, i32, nil, "copy from a bounded host byte array"},
		{"syscall/js.copyBytesToJS", p.goCopyBytesToJS, i32, nil, "copy into a bounded host byte array"},
	}
	for _, binding := range bindings {
		m.Func(binding.name, binding.fn).Params(binding.params...).Results(binding.results...).Docs(binding.docs)
	}
	return nil
}

func goMemory(module wago.HostModule, params []uint64) ([]byte, uint32, bool) {
	if len(params) == 0 {
		return nil, 0, false
	}
	return module.Memory(), uint32(params[0]), true
}

func memoryRange(memory []byte, offset uint32, size uint64) ([]byte, bool) {
	end := uint64(offset) + size
	if end > uint64(len(memory)) {
		return nil, false
	}
	return memory[offset:uint32(end)], true
}

func readGoI64(memory []byte, offset uint32) (int64, bool) {
	b, ok := memoryRange(memory, offset, 8)
	if !ok {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(b)), true
}

func writeGoI64(memory []byte, offset uint32, value int64) bool {
	b, ok := memoryRange(memory, offset, 8)
	if ok {
		binary.LittleEndian.PutUint64(b, uint64(value))
	}
	return ok
}

func (p *plugin) ensureGoState(module wago.HostModule) {
	_, _ = p.goStateFor(module)
}

func (p *plugin) goStateFor(module wago.HostModule) (*goState, bool) {
	id, err := p.callers.Resolve(module)
	if err != nil {
		return nil, false
	}
	p.mu.Lock()
	state := p.states[id]
	if state == nil {
		state = newGoState()
		p.states[id] = state
	}
	p.mu.Unlock()
	return state, true
}

func newGoState() *goState {
	number := func(value float64) *jsValue { return &jsValue{kind: jsNumber, number: value} }
	function := func(fn func([]*jsValue) (*jsValue, error)) *jsValue { return &jsValue{kind: jsFunction, call: fn} }
	constants := &jsValue{kind: jsObject, object: map[string]*jsValue{}}
	for _, name := range []string{"O_WRONLY", "O_RDWR", "O_CREAT", "O_TRUNC", "O_APPEND", "O_EXCL"} {
		constants.object[name] = number(-1)
	}
	fs := &jsValue{kind: jsObject, object: map[string]*jsValue{"constants": constants}}
	process := &jsValue{kind: jsObject, object: map[string]*jsValue{
		"pid": number(-1), "ppid": number(-1),
	}}
	for _, name := range []string{"getuid", "getgid", "geteuid", "getegid"} {
		process.object[name] = function(func([]*jsValue) (*jsValue, error) { return number(-1), nil })
	}
	process.object["cwd"] = function(func([]*jsValue) (*jsValue, error) { return &jsValue{kind: jsString, text: "/"}, nil })
	unsupported := function(func([]*jsValue) (*jsValue, error) { return nil, errors.New("ENOSYS") })
	for _, name := range []string{"read", "open", "close", "stat", "fstat", "lstat", "readdir", "readlink", "mkdir", "rmdir", "rename", "unlink", "chmod", "chown", "fchmod", "fchown", "fsync", "ftruncate", "link", "symlink", "truncate", "utimes"} {
		fs.object[name] = unsupported
	}
	// Go's callback wrapper is synchronously resumed while the originating host
	// call is active. The bounded read/write bridge below handles these methods.
	fs.object["write"] = function(func([]*jsValue) (*jsValue, error) { return &jsValue{kind: jsUndefined}, nil })
	fs.object["writeSync"] = function(func(args []*jsValue) (*jsValue, error) {
		if len(args) > 1 && args[1] != nil && args[1].kind == jsBytes {
			return number(float64(len(args[1].bytes))), nil
		}
		return number(0), nil
	})
	global := &jsValue{kind: jsObject, object: map[string]*jsValue{}}
	global.object["fs"] = fs
	global.object["process"] = process
	global.object["Uint8Array"] = function(func(args []*jsValue) (*jsValue, error) {
		length := 0
		if len(args) != 0 && args[0] != nil && args[0].kind == jsNumber && args[0].number > 0 && args[0].number <= 1<<30 {
			length = int(args[0].number)
		}
		return &jsValue{kind: jsBytes, bytes: make([]byte, length)}, nil
	})
	global.object["crypto"] = &jsValue{kind: jsObject, object: map[string]*jsValue{}}
	global.object["performance"] = &jsValue{kind: jsObject, object: map[string]*jsValue{
		"now": function(func([]*jsValue) (*jsValue, error) { return number(float64(time.Now().UnixNano()) / 1e6), nil }),
	}}
	goObject := &jsValue{kind: jsObject, object: map[string]*jsValue{}}
	goObject.object["_makeFuncWrapper"] = function(func(args []*jsValue) (*jsValue, error) {
		callback := &jsValue{kind: jsFunction}
		if len(args) != 0 && args[0] != nil && args[0].kind == jsNumber {
			callback.callbackID = uint32(args[0].number)
		}
		return callback, nil
	})
	state := &goState{values: map[uint32]*jsValue{
		0: {kind: jsNumber, number: math.NaN()}, 1: number(0), 2: {kind: jsNull},
		3: {kind: jsBoolean, truth: true}, 4: {kind: jsBoolean}, 5: global, 6: goObject,
	}, nextID: 7}
	return state
}

func (p *plugin) goResetMemory(module wago.HostModule, _ []uint64, _ []uint64) {
	p.ensureGoState(module)
}

func (p *plugin) goExit(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		panic(wago.HostExit{})
	}
	b, ok := memoryRange(memory, sp+8, 4)
	if !ok {
		panic(wago.HostExit{})
	}
	panic(wago.HostExit{Code: int32(binary.LittleEndian.Uint32(b))})
}

func (p *plugin) goWrite(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		return
	}
	fd, okFD := readGoI64(memory, sp+8)
	ptr, okPtr := readGoI64(memory, sp+16)
	nb, okN := memoryRange(memory, sp+24, 4)
	if !okFD || !okPtr || !okN || ptr < 0 {
		return
	}
	n := binary.LittleEndian.Uint32(nb)
	data, ok := memoryRange(memory, uint32(ptr), uint64(n))
	if !ok {
		return
	}
	writer := p.stderr
	if fd == 1 {
		writer = p.stdout
	}
	_, _ = writer.Write(data)
}

func (p *plugin) goNanotime(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if ok {
		writeGoI64(memory, sp+8, time.Now().UnixNano())
	}
}

func (p *plugin) goWalltime(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		return
	}
	now := time.Now()
	if !writeGoI64(memory, sp+8, now.Unix()) {
		return
	}
	if b, ok := memoryRange(memory, sp+16, 4); ok {
		binary.LittleEndian.PutUint32(b, uint32(now.Nanosecond()))
	}
}

func (p *plugin) goScheduleTimeout(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		return
	}
	id := nextTimeoutID.Add(1)
	if b, ok := memoryRange(memory, sp+16, 4); ok {
		binary.LittleEndian.PutUint32(b, id)
	}
}

func (p *plugin) goRandom(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		return
	}
	ptr, okPtr := readGoI64(memory, sp+8)
	length, okLen := readGoI64(memory, sp+16)
	if !okPtr || !okLen || ptr < 0 || length < 0 {
		return
	}
	b, ok := memoryRange(memory, uint32(ptr), uint64(length))
	if ok {
		_, _ = rand.Read(b)
	}
}

func loadGoString(memory []byte, descriptor uint32) (string, bool) {
	ptr, okPtr := readGoI64(memory, descriptor)
	length, okLen := readGoI64(memory, descriptor+8)
	if !okPtr || !okLen || ptr < 0 || length < 0 {
		return "", false
	}
	b, ok := memoryRange(memory, uint32(ptr), uint64(length))
	return string(b), ok
}

func loadGoValues(state *goState, memory []byte, descriptor uint32) ([]*jsValue, bool) {
	ptr, okPtr := readGoI64(memory, descriptor)
	length, okLen := readGoI64(memory, descriptor+8)
	if !okPtr || !okLen || ptr < 0 || length < 0 || length > 1<<20 {
		return nil, false
	}
	values := make([]*jsValue, int(length))
	for i := range values {
		values[i] = state.loadValue(memory, uint32(ptr)+uint32(i)*8)
	}
	return values, true
}

func (s *goState) loadValue(memory []byte, offset uint32) *jsValue {
	b, ok := memoryRange(memory, offset, 8)
	if !ok {
		return nil
	}
	bits := binary.LittleEndian.Uint64(b)
	number := math.Float64frombits(bits)
	if bits == 0 {
		return &jsValue{kind: jsUndefined}
	}
	if !math.IsNaN(number) {
		return &jsValue{kind: jsNumber, number: number}
	}
	return s.values[uint32(bits)]
}

func (s *goState) storeValue(memory []byte, offset uint32, value *jsValue) bool {
	b, ok := memoryRange(memory, offset, 8)
	if !ok {
		return false
	}
	if value == nil || value.kind == jsUndefined {
		binary.LittleEndian.PutUint64(b, 0)
		return true
	}
	if value.kind == jsNumber && value.number != 0 && !math.IsNaN(value.number) {
		binary.LittleEndian.PutUint64(b, math.Float64bits(value.number))
		return true
	}
	id := s.nextID
	typeFlag := uint32(0)
	switch value.kind {
	case jsNull:
		id = 2
	case jsBoolean:
		if value.truth {
			id = 3
		} else {
			id = 4
		}
	case jsNumber:
		if value.number == 0 {
			id = 1
		} else {
			id = 0
		}
	case jsObject, jsArray, jsBytes:
		typeFlag = 1
		s.values[id] = value
		s.nextID++
	case jsString:
		typeFlag = 2
		s.values[id] = value
		s.nextID++
	case jsFunction:
		typeFlag = 4
		s.values[id] = value
		s.nextID++
	default:
		binary.LittleEndian.PutUint64(b, 0)
		return true
	}
	binary.LittleEndian.PutUint32(b[:4], id)
	binary.LittleEndian.PutUint32(b[4:], 0x7ff80000|typeFlag)
	return true
}

func (p *plugin) goStringVal(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	text, _ := loadGoString(memory, sp+8)
	state.mu.Lock()
	state.storeValue(memory, sp+24, &jsValue{kind: jsString, text: text})
	state.mu.Unlock()
}

func (p *plugin) goValueGet(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	property, _ := loadGoString(memory, sp+16)
	state.mu.Lock()
	value := state.loadValue(memory, sp+8)
	var result *jsValue
	if value != nil && value.kind == jsObject {
		result = value.object[property]
	}
	state.storeValue(memory, sp+32, result)
	state.mu.Unlock()
}

func (p *plugin) goValueSet(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	property, _ := loadGoString(memory, sp+16)
	state.mu.Lock()
	target := state.loadValue(memory, sp+8)
	value := state.loadValue(memory, sp+32)
	if target != nil && target.kind == jsObject {
		target.object[property] = value
	}
	state.mu.Unlock()
}

func (p *plugin) goValueIndex(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	index, _ := readGoI64(memory, sp+16)
	state.mu.Lock()
	value := state.loadValue(memory, sp+8)
	var result *jsValue
	if value != nil && index >= 0 {
		switch value.kind {
		case jsArray:
			if index < int64(len(value.array)) {
				result = value.array[index]
			}
		case jsBytes:
			if index < int64(len(value.bytes)) {
				result = &jsValue{kind: jsNumber, number: float64(value.bytes[index])}
			}
		case jsString:
			if index < int64(len(value.text)) {
				result = &jsValue{kind: jsString, text: value.text[index : index+1]}
			}
		}
	}
	state.storeValue(memory, sp+24, result)
	state.mu.Unlock()
}

func (p *plugin) goValueSetIndex(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	index, _ := readGoI64(memory, sp+16)
	state.mu.Lock()
	target := state.loadValue(memory, sp+8)
	value := state.loadValue(memory, sp+24)
	if target != nil && index >= 0 {
		switch target.kind {
		case jsArray:
			if index < int64(len(target.array)) {
				target.array[index] = value
			}
		case jsBytes:
			if index < int64(len(target.bytes)) && value != nil && value.kind == jsNumber {
				target.bytes[index] = byte(value.number)
			}
		}
	}
	state.mu.Unlock()
}

func (p *plugin) goValueCall(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	method, _ := loadGoString(memory, sp+16)
	state.mu.Lock()
	receiver := state.loadValue(memory, sp+8)
	args, argsOK := loadGoValues(state, memory, sp+32)
	if argsOK && p.goAsyncFSCall(module, state, receiver, method, args) {
		state.storeValue(memory, sp+56, &jsValue{kind: jsUndefined})
		if b, ok := memoryRange(memory, sp+64, 1); ok {
			b[0] = 1
		}
		state.mu.Unlock()
		return
	}
	var result *jsValue
	var callErr error
	if receiver == nil || receiver.kind != jsObject || receiver.object[method] == nil || receiver.object[method].call == nil || !argsOK {
		callErr = errors.New("JavaScript method unavailable")
	} else {
		result, callErr = receiver.object[method].call(args)
	}
	if callErr != nil {
		result = &jsValue{kind: jsString, text: callErr.Error()}
	}
	state.storeValue(memory, sp+56, result)
	if b, ok := memoryRange(memory, sp+64, 1); ok {
		if callErr == nil {
			b[0] = 1
		} else {
			b[0] = 0
		}
	}
	state.mu.Unlock()
}

func (p *plugin) goAsyncFSCall(module wago.HostModule, state *goState, receiver *jsValue, method string, args []*jsValue) bool {
	global := state.values[5]
	if global == nil || receiver != global.object["fs"] || (method != "write" && method != "read") || len(args) == 0 {
		return false
	}
	callback := args[len(args)-1]
	if callback == nil || callback.kind != jsFunction || callback.callbackID == 0 {
		return false
	}
	count := 0
	callbackArgs := []*jsValue{{kind: jsNull}}
	if len(args) >= 4 && args[1] != nil && args[1].kind == jsBytes {
		offset, length := 0, len(args[1].bytes)
		if args[2] != nil && args[2].kind == jsNumber {
			offset = int(args[2].number)
		}
		if args[3] != nil && args[3].kind == jsNumber {
			length = int(args[3].number)
		}
		if offset >= 0 && length >= 0 && offset <= len(args[1].bytes) && length <= len(args[1].bytes)-offset {
			if method == "read" {
				count, _ = p.stdin.Read(args[1].bytes[offset : offset+length])
			} else {
				fd := 1
				if args[0] != nil && args[0].kind == jsNumber {
					fd = int(args[0].number)
				}
				writer := p.stderr
				if fd == 1 {
					writer = p.stdout
				}
				count, _ = writer.Write(args[1].bytes[offset : offset+length])
			}
		}
	}
	callbackArgs = append(callbackArgs, &jsValue{kind: jsNumber, number: float64(count)})
	goObject := state.values[6]
	goObject.object["_pendingEvent"] = &jsValue{kind: jsObject, object: map[string]*jsValue{
		"id":   {kind: jsNumber, number: float64(callback.callbackID)},
		"this": {kind: jsUndefined},
		"args": {kind: jsArray, array: callbackArgs},
	}}
	state.mu.Unlock()
	_, err := p.invoker.Invoke(context.Background(), module, "resume")
	state.mu.Lock()
	if err != nil {
		var exit *wago.ExitError
		if !errors.As(err, &exit) || exit.Code != 0 {
			panic(wago.HostTrap{Err: fmt.Errorf("gojs: resume callback: %w", err)})
		}
	}
	return true
}

func (p *plugin) goValueNew(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	state.mu.Lock()
	constructor := state.loadValue(memory, sp+8)
	args, argsOK := loadGoValues(state, memory, sp+16)
	var result *jsValue
	var callErr error
	if constructor == nil || constructor.call == nil || !argsOK {
		callErr = errors.New("JavaScript constructor unavailable")
	} else {
		result, callErr = constructor.call(args)
	}
	if callErr != nil {
		result = &jsValue{kind: jsString, text: callErr.Error()}
	}
	state.storeValue(memory, sp+40, result)
	if b, ok := memoryRange(memory, sp+48, 1); ok {
		if callErr == nil {
			b[0] = 1
		}
	}
	state.mu.Unlock()
}

func (p *plugin) goValueLength(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	state.mu.Lock()
	value := state.loadValue(memory, sp+8)
	length := 0
	if value != nil {
		if value.kind == jsBytes {
			length = len(value.bytes)
		} else if value.kind == jsArray {
			length = len(value.array)
		} else if value.kind == jsString {
			length = len(value.text)
		}
	}
	writeGoI64(memory, sp+16, int64(length))
	state.mu.Unlock()
}

func goStoreUndefined(relative uint32) wago.HostFunc {
	return func(module wago.HostModule, params, _ []uint64) {
		memory, sp, ok := goMemory(module, params)
		if ok {
			writeGoI64(memory, sp+relative, 0)
		}
	}
}

func goStoreI64(relative uint32, value int64) wago.HostFunc {
	return func(module wago.HostModule, params, _ []uint64) {
		memory, sp, ok := goMemory(module, params)
		if ok {
			writeGoI64(memory, sp+relative, value)
		}
	}
}

func goFailedCall(valueOffset, okOffset uint32) wago.HostFunc {
	return func(module wago.HostModule, params, _ []uint64) {
		memory, sp, ok := goMemory(module, params)
		if !ok {
			return
		}
		writeGoI64(memory, sp+valueOffset, 0)
		if b, ok := memoryRange(memory, sp+okOffset, 1); ok {
			b[0] = 0
		}
	}
}

func (p *plugin) goPrepareString(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	state.mu.Lock()
	value := state.loadValue(memory, sp+8)
	text := "<undefined>"
	if value != nil {
		switch value.kind {
		case jsString:
			text = value.text
		case jsNumber:
			text = fmt.Sprint(value.number)
		case jsBoolean:
			text = fmt.Sprint(value.truth)
		case jsNull:
			text = "null"
		}
	}
	encoded := &jsValue{kind: jsBytes, bytes: []byte(text)}
	state.storeValue(memory, sp+16, encoded)
	writeGoI64(memory, sp+24, int64(len(encoded.bytes)))
	state.mu.Unlock()
}

func (p *plugin) goLoadString(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	state.mu.Lock()
	value := state.loadValue(memory, sp+8)
	ptr, okPtr := readGoI64(memory, sp+16)
	length, okLen := readGoI64(memory, sp+24)
	if value != nil && value.kind == jsBytes && okPtr && okLen && ptr >= 0 && length >= 0 {
		if destination, ok := memoryRange(memory, uint32(ptr), uint64(length)); ok {
			copy(destination, value.bytes)
		}
	}
	state.mu.Unlock()
}

func (p *plugin) goCopyBytesToGo(module wago.HostModule, params, _ []uint64) {
	p.goCopyBytes(module, params, true)
}

func (p *plugin) goCopyBytesToJS(module wago.HostModule, params, _ []uint64) {
	p.goCopyBytes(module, params, false)
}

func (p *plugin) goCopyBytes(module wago.HostModule, params []uint64, toGo bool) {
	memory, sp, ok := goMemory(module, params)
	state, stateOK := p.goStateFor(module)
	if !ok || !stateOK {
		return
	}
	state.mu.Lock()
	var host *jsValue
	var ptr, length int64
	if toGo {
		host = state.loadValue(memory, sp+32)
		ptr, _ = readGoI64(memory, sp+8)
		length, _ = readGoI64(memory, sp+16)
	} else {
		host = state.loadValue(memory, sp+8)
		ptr, _ = readGoI64(memory, sp+16)
		length, _ = readGoI64(memory, sp+24)
	}
	success := false
	copied := 0
	if host != nil && host.kind == jsBytes && ptr >= 0 && length >= 0 {
		if guest, ok := memoryRange(memory, uint32(ptr), uint64(length)); ok {
			if toGo {
				copied = copy(guest, host.bytes)
			} else {
				copied = copy(host.bytes, guest)
			}
			success = true
		}
	}
	writeGoI64(memory, sp+40, int64(copied))
	if b, ok := memoryRange(memory, sp+48, 1); ok && success {
		b[0] = 1
	}
	state.mu.Unlock()
}

func goFailedCopy(module wago.HostModule, params, _ []uint64) {
	memory, sp, ok := goMemory(module, params)
	if !ok {
		return
	}
	writeGoI64(memory, sp+40, 0)
	if b, ok := memoryRange(memory, sp+48, 1); ok {
		b[0] = 0
	}
}
