package emscripten

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

var wasmHeader = []byte{'\x00', 'a', 's', 'm', '\x01', '\x00', '\x00', '\x00'}

type rawSection struct {
	id      byte
	payload []byte
}

type importRewrite struct {
	sections      []rawSection
	functionCount uint32
	functionTypes map[string]uint32
	invokes       map[string]uint32
	memory        []byte
}

func transformModule(source []byte, runtimeArgs []string) ([]byte, error) {
	return transformModuleWithEnvironment(source, runtimeArgs, nil)
}

func transformModuleWithEnvironment(source []byte, runtimeArgs, environment []string) ([]byte, error) {
	kind := classifyModule(source)
	if kind == "" {
		return source, nil
	}
	sections, err := decodeSections(source)
	if err != nil {
		return nil, fmt.Errorf("emscripten: decode %s module: %w", kind, err)
	}
	rewrite, err := rewriteImports(sections, kind)
	if err != nil {
		return nil, fmt.Errorf("emscripten: rewrite %s imports: %w", kind, err)
	}
	sections = rewrite.sections
	invokeNames := make([]string, 0, len(rewrite.invokes))
	for name := range rewrite.invokes {
		invokeNames = append(invokeNames, name)
	}
	sort.Strings(invokeNames)
	for _, name := range invokeNames {
		typeIndex := rewrite.invokes[name]
		sections, err = addInvokeWrapper(sections, rewrite.functionCount, name, typeIndex)
		if err != nil {
			return nil, fmt.Errorf("emscripten: add %s callback wrapper: %w", name, err)
		}
	}
	if len(rewrite.memory) != 0 {
		sections, err = defineImportedMemory(sections, rewrite.memory)
		if err != nil {
			return nil, fmt.Errorf("emscripten: internalize memory: %w", err)
		}
	}
	if _, ok := rewrite.functionTypes["env.emscripten_resize_heap"]; ok {
		sections, err = addResizeHeapWrapper(sections, rewrite.functionCount)
		if err != nil {
			return nil, fmt.Errorf("emscripten: add heap resize wrapper: %w", err)
		}
	}
	targets := []string{"__wasm_call_ctors"}
	var argv []string
	switch kind {
	case "ruby":
		targets = []string{"_initialize", "ruby-show-version: func() -> ()"}
	case "gojs":
		targets = []string{"run"}
		argv = append([]string(nil), runtimeArgs...)
		if len(argv) == 0 {
			argv = []string{"wasm"}
		}
	case "emscripten":
		targets = nil
	}
	if kind == "emscripten" {
		sections, err = addEmscriptenLauncher(sections, rewrite.functionCount, runtimeArgs, environment)
	} else {
		sections, err = addStartLauncher(sections, rewrite.functionCount, targets, argv)
	}
	if err != nil {
		return nil, fmt.Errorf("emscripten: add %s launcher: %w", kind, err)
	}
	return encodeSections(sections), nil
}

func classifyModule(source []byte) string {
	switch {
	case bytes.Contains(source, []byte("rb-js-abi-host")) && bytes.Contains(source, []byte("ruby-show-version")):
		return "ruby"
	case bytes.Contains(source, []byte("runtime.wasmExit")) && bytes.Contains(source, []byte("syscall/js.valueGet")):
		return "gojs"
	case bytes.Contains(source, []byte("sqlite3_initialize")) && bytes.Contains(source, []byte("emscripten_resize_heap")):
		return "sqlite"
	case bytes.Contains(source, []byte("luaL_newstate")) && bytes.Contains(source, []byte("invoke_vii")):
		return "lua"
	case looksLikeEmscripten(source):
		return "emscripten"
	default:
		return ""
	}
}

func looksLikeEmscripten(source []byte) bool {
	if !bytes.Contains(source, []byte("env")) {
		return false
	}
	// Embind/emval and application-specific JavaScript APIs require their
	// generated glue. Treating their names as generic Emscripten imports would
	// hide that dependency and produce a module that cannot execute correctly.
	for _, glueMarker := range [][]byte{[]byte("_embind_"), []byte("_emval_"), []byte("duckdb_web_")} {
		if bytes.Contains(source, glueMarker) {
			return false
		}
	}
	hasMain := bytes.Contains(source, []byte{'\x04', 'm', 'a', 'i', 'n', '\x00'}) ||
		bytes.Contains(source, []byte{'\x05', '_', 'm', 'a', 'i', 'n', '\x00'}) ||
		bytes.Contains(source, appendName(nil, "__main_argc_argv"))
	if !hasMain {
		return false
	}
	for _, marker := range [][]byte{
		[]byte("emscripten_resize_heap"), []byte("__wasm_call_ctors"),
		[]byte("_emscripten_stack_alloc"), []byte("_tzset_js"),
		[]byte("__syscall_"), []byte("invoke_"),
	} {
		if bytes.Contains(source, marker) {
			return true
		}
	}
	return false
}

func decodeSections(source []byte) ([]rawSection, error) {
	if len(source) < len(wasmHeader) || !bytes.Equal(source[:len(wasmHeader)], wasmHeader) {
		return nil, fmt.Errorf("invalid WebAssembly header")
	}
	var sections []rawSection
	for offset := len(wasmHeader); offset < len(source); {
		id := source[offset]
		offset++
		size, n, err := readU32(source[offset:])
		if err != nil {
			return nil, err
		}
		offset += n
		end := uint64(offset) + uint64(size)
		if end > uint64(len(source)) {
			return nil, fmt.Errorf("section %d exceeds module", id)
		}
		sections = append(sections, rawSection{id: id, payload: append([]byte(nil), source[offset:int(end)]...)})
		offset = int(end)
	}
	return sections, nil
}

func encodeSections(sections []rawSection) []byte {
	out := append([]byte(nil), wasmHeader...)
	for _, section := range sections {
		out = append(out, section.id)
		out = appendU32(out, uint32(len(section.payload)))
		out = append(out, section.payload...)
	}
	return out
}

func rewriteImports(sections []rawSection, kind string) (importRewrite, error) {
	result := importRewrite{sections: sections, functionTypes: make(map[string]uint32), invokes: make(map[string]uint32)}
	index := sectionIndex(sections, 2)
	if index < 0 {
		return result, nil
	}
	payload := sections[index].payload
	count, n, err := readU32(payload)
	if err != nil {
		return result, err
	}
	offset := n
	entries := make([][]byte, 0, count)
	for i := uint32(0); i < count; i++ {
		module, moduleEnd, err := readName(payload, offset)
		if err != nil {
			return result, err
		}
		name, nameEnd, err := readName(payload, moduleEnd)
		if err != nil {
			return result, err
		}
		if nameEnd >= len(payload) {
			return result, fmt.Errorf("truncated import descriptor")
		}
		kindByte := payload[nameEnd]
		descEnd, err := importDescriptorEnd(payload, nameEnd+1, kindByte)
		if err != nil {
			return result, fmt.Errorf("import %s.%s: %w", module, name, err)
		}
		var importType uint32
		if kindByte == 0 {
			result.functionCount++
			importType, _, _ = readU32(payload[nameEnd+1 : descEnd])
			result.functionTypes[module+"."+name] = importType
			if module == "env" && len(name) > len("invoke_") && name[:len("invoke_")] == "invoke_" {
				if !supportsInvoke(name) {
					return result, fmt.Errorf("unsupported callback signature %q", name)
				}
				result.invokes[name] = importType
			}
		}
		if kind != "gojs" && kind != "ruby" && module == "env" && name == "memory" && kindByte == 2 {
			if len(result.memory) != 0 {
				return result, fmt.Errorf("multiple env.memory imports")
			}
			result.memory = append([]byte(nil), payload[nameEnd+1:descEnd]...)
			offset = descEnd
			continue
		}
		if kind != "gojs" && kind != "ruby" && module == "env" {
			switch name {
			case "_localtime_js":
				params, _, typeErr := functionType(sections, importType)
				if typeErr != nil {
					return result, typeErr
				}
				if len(params) == 2 && params[0] == 0x7e {
					name = "__wago_localtime_js_i64"
				}
			case "_gmtime_js":
				params, _, typeErr := functionType(sections, importType)
				if typeErr != nil {
					return result, typeErr
				}
				if len(params) == 2 && params[0] == 0x7e {
					name = "__wago_gmtime_js_i64"
				}
			case "_tzset_js":
				params, _, typeErr := functionType(sections, importType)
				if typeErr != nil {
					return result, typeErr
				}
				if len(params) == 4 {
					name = "__wago_tzset_js_4"
				}
			case "_munmap_js", "_mmap_js":
				params, _, typeErr := functionType(sections, importType)
				if typeErr != nil {
					return result, typeErr
				}
				if bytes.Contains(params, []byte{0x7e}) {
					name = "__wago" + name + "_i64"
				}
			}
		}
		if kind != "gojs" && kind != "ruby" && module == "wasi_snapshot_preview1" {
			switch name {
			case "fd_read", "fd_write", "fd_close", "fd_seek", "fd_fdstat_get", "fd_filestat_get",
				"fd_sync", "fd_datasync", "fd_fdstat_set_flags", "fd_filestat_set_size",
				"fd_pread", "fd_pwrite", "fd_advise", "fd_allocate", "environ_sizes_get", "environ_get":
				module = "env"
				name = "__wago_wasi_" + name
			}
		}
		entry := appendName(nil, module)
		entry = appendName(entry, name)
		entry = append(entry, payload[nameEnd:descEnd]...)
		entries = append(entries, entry)
		offset = descEnd
	}
	if offset != len(payload) {
		return result, fmt.Errorf("trailing import bytes")
	}
	updated := appendU32(nil, uint32(len(entries)))
	for _, entry := range entries {
		updated = append(updated, entry...)
	}
	result.sections = append([]rawSection(nil), sections...)
	result.sections[index].payload = updated
	return result, nil
}

func addInvokeWrapper(sections []rawSection, importedFunctions uint32, name string, wrapperType uint32) ([]rawSection, error) {
	params, results, err := functionType(sections, wrapperType)
	if err != nil {
		return nil, err
	}
	if len(params) == 0 || params[0] != 0x7f {
		return nil, fmt.Errorf("callback import must start with an i32 table index")
	}
	callbackType, err := findFunctionType(sections, params[1:], results)
	if err != nil {
		return nil, err
	}
	definedFunctions, err := appendVectorItem(sections, 3, appendU32(nil, wrapperType))
	if err != nil {
		return nil, err
	}
	functionIndex := importedFunctions + definedFunctions
	exportEntry := appendName(nil, "__wago_"+name)
	exportEntry = append(exportEntry, 0x00)
	exportEntry = appendU32(exportEntry, functionIndex)
	if _, err := appendVectorItem(sections, 7, exportEntry); err != nil {
		return nil, err
	}
	body := []byte{0x00}
	for i := 1; i < len(params); i++ {
		body = append(body, 0x20)
		body = appendU32(body, uint32(i))
	}
	body = append(body, 0x20, 0x00, 0x11)
	body = appendU32(body, callbackType)
	body = append(body, 0x00, 0x0b)
	code := appendU32(nil, uint32(len(body)))
	code = append(code, body...)
	if _, err := appendVectorItem(sections, 10, code); err != nil {
		return nil, err
	}
	return sections, nil
}

func functionType(sections []rawSection, wanted uint32) ([]byte, []byte, error) {
	index := sectionIndex(sections, 1)
	if index < 0 {
		return nil, nil, fmt.Errorf("missing type section")
	}
	payload := sections[index].payload
	count, n, err := readU32(payload)
	if err != nil {
		return nil, nil, err
	}
	if wanted >= count {
		return nil, nil, fmt.Errorf("function type %d exceeds %d types", wanted, count)
	}
	offset := n
	for typeIndex := uint32(0); typeIndex < count; typeIndex++ {
		if offset >= len(payload) || payload[offset] != 0x60 {
			return nil, nil, fmt.Errorf("unsupported non-function type at index %d", typeIndex)
		}
		offset++
		paramCount, size, err := readU32(payload[offset:])
		if err != nil {
			return nil, nil, err
		}
		offset += size
		paramEnd := offset + int(paramCount)
		if paramEnd > len(payload) {
			return nil, nil, fmt.Errorf("truncated function parameters")
		}
		params := payload[offset:paramEnd]
		offset = paramEnd
		resultCount, size, err := readU32(payload[offset:])
		if err != nil {
			return nil, nil, err
		}
		offset += size
		resultEnd := offset + int(resultCount)
		if resultEnd > len(payload) {
			return nil, nil, fmt.Errorf("truncated function results")
		}
		results := payload[offset:resultEnd]
		offset = resultEnd
		if typeIndex == wanted {
			return append([]byte(nil), params...), append([]byte(nil), results...), nil
		}
	}
	return nil, nil, fmt.Errorf("function type %d not found", wanted)
}

func findFunctionType(sections []rawSection, params, results []byte) (uint32, error) {
	index := sectionIndex(sections, 1)
	if index < 0 {
		return 0, fmt.Errorf("missing type section")
	}
	payload := sections[index].payload
	count, n, err := readU32(payload)
	if err != nil {
		return 0, err
	}
	offset := n
	for typeIndex := uint32(0); typeIndex < count; typeIndex++ {
		if offset >= len(payload) || payload[offset] != 0x60 {
			return 0, fmt.Errorf("unsupported non-function type at index %d", typeIndex)
		}
		offset++
		paramCount, n, err := readU32(payload[offset:])
		if err != nil {
			return 0, err
		}
		offset += n
		paramEnd := offset + int(paramCount)
		if paramEnd > len(payload) {
			return 0, fmt.Errorf("truncated function parameters")
		}
		gotParams := payload[offset:paramEnd]
		offset = paramEnd
		resultCount, n, err := readU32(payload[offset:])
		if err != nil {
			return 0, err
		}
		offset += n
		resultEnd := offset + int(resultCount)
		if resultEnd > len(payload) {
			return 0, fmt.Errorf("truncated function results")
		}
		gotResults := payload[offset:resultEnd]
		offset = resultEnd
		if bytes.Equal(gotParams, params) && bytes.Equal(gotResults, results) {
			return typeIndex, nil
		}
	}
	return 0, fmt.Errorf("callback function type not found")
}

func defineImportedMemory(sections []rawSection, descriptor []byte) ([]rawSection, error) {
	if sectionIndex(sections, 5) >= 0 {
		return nil, fmt.Errorf("module already defines a memory")
	}
	memory := rawSection{id: 5, payload: append(appendU32(nil, 1), descriptor...)}
	insert := len(sections)
	for i, section := range sections {
		if section.id != 0 && section.id > 5 {
			insert = i
			break
		}
	}
	sections = append(sections, rawSection{})
	copy(sections[insert+1:], sections[insert:])
	sections[insert] = memory
	return sections, nil
}

func addResizeHeapWrapper(sections []rawSection, importedFunctions uint32) ([]rawSection, error) {
	typeIndex, err := findFunctionType(sections, []byte{0x7f}, []byte{0x7f})
	if err != nil {
		return nil, err
	}
	definedFunctions, err := appendVectorItem(sections, 3, appendU32(nil, typeIndex))
	if err != nil {
		return nil, err
	}
	functionIndex := importedFunctions + definedFunctions
	exportEntry := appendName(nil, "__wago_resize_heap")
	exportEntry = append(exportEntry, 0x00)
	exportEntry = appendU32(exportEntry, functionIndex)
	if _, err := appendVectorItem(sections, 7, exportEntry); err != nil {
		return nil, err
	}

	// target = ceil(requestedBytes / 64KiB); grow only by the missing pages.
	body := []byte{0x01, 0x02, 0x7f, 0x20, 0x00, 0x41}
	body = appendS32(body, 65535)
	body = append(body,
		0x6a, 0x41, 0x10, 0x76, 0x21, 0x02, // add, 16, shr_u, local.set target
		0x3f, 0x00, 0x21, 0x01, // memory.size, local.set current
		0x20, 0x02, 0x20, 0x01, 0x4d, // target <= current
		0x04, 0x7f, 0x41, 0x01, // if (result i32), true
		0x05, 0x20, 0x02, 0x20, 0x01, 0x6b, 0x40, 0x00, // else memory.grow(target-current)
		0x41, 0x7f, 0x47, // != -1
		0x0b, 0x0b,
	)
	code := appendU32(nil, uint32(len(body)))
	code = append(code, body...)
	if _, err := appendVectorItem(sections, 10, code); err != nil {
		return nil, err
	}
	return sections, nil
}

func addEmscriptenLauncher(sections []rawSection, importedFunctions uint32, argv, environment []string) ([]rawSection, error) {
	exports, hasStart, err := exportedFunctions(sections)
	if err != nil || hasStart {
		return sections, err
	}
	var target uint32
	var ok bool
	for _, name := range []string{"__main_argc_argv", "main", "_main"} {
		if target, ok = exports[name]; ok {
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("missing a conventional Emscripten main export")
	}
	ctors, hasCtors := exports["__wasm_call_ctors"]
	params, results, err := functionTypeForIndex(sections, importedFunctions, target)
	if err != nil {
		return nil, err
	}
	if len(results) > 1 || len(results) == 1 && results[0] != 0x7f {
		return nil, fmt.Errorf("main has unsupported result signature")
	}
	if len(params) != 0 && !(len(params) == 2 && params[0] == 0x7f && params[1] == 0x7f) {
		return nil, fmt.Errorf("main has unsupported parameter signature")
	}
	stackCurrent, hasStackCurrent := exports["emscripten_stack_get_current"]
	stackAlloc, hasStackAlloc := exports["_emscripten_stack_alloc"]
	stackRestore, hasStackRestore := exports["_emscripten_stack_restore"]
	useArgv := len(params) == 2 && len(argv) != 0 && hasStackCurrent && hasStackAlloc && hasStackRestore
	var argData []byte
	var stringOffsets []uint32
	var pointerOffset, allocationSize uint32
	if useArgv {
		argData, stringOffsets, pointerOffset, allocationSize, err = encodeEmscriptenArgv(argv, environment)
		if err != nil {
			return nil, err
		}
	}

	typeIndex, err := findFunctionType(sections, nil, nil)
	if err != nil {
		typeIndex, err = appendVectorItem(sections, 1, []byte{0x60, 0x00, 0x00})
		if err != nil {
			return nil, err
		}
	}
	definedFunctions, err := appendVectorItem(sections, 3, appendU32(nil, typeIndex))
	if err != nil {
		return nil, err
	}
	launcherIndex := importedFunctions + definedFunctions
	exportEntry := appendName(nil, "_start")
	exportEntry = append(exportEntry, 0x00)
	exportEntry = appendU32(exportEntry, launcherIndex)
	if _, err := appendVectorItem(sections, 7, exportEntry); err != nil {
		return nil, err
	}
	body := []byte{0x00}
	if hasCtors {
		body = append(body, 0x10)
		body = appendU32(body, ctors)
	}
	if useArgv {
		body = append([]byte{0x01, 0x03, 0x7f}, body[1:]...) // three i32 locals.
		body = append(body, 0x10)                            // save the current stack.
		body = appendU32(body, stackCurrent)
		body = append(body, 0x21, 0x00, 0x41)
		body = appendS32(body, int32(allocationSize))
		body = append(body, 0x10)
		body = appendU32(body, stackAlloc)
		body = append(body, 0x21, 0x01)
		for offset, value := range argData {
			body = append(body, 0x20, 0x01, 0x41)
			body = appendS32(body, int32(value))
			body = append(body, 0x3a, 0x00)
			body = appendU32(body, uint32(offset))
		}
		for i, offset := range stringOffsets {
			body = append(body, 0x20, 0x01)
			if offset == ^uint32(0) {
				body = append(body, 0x41, 0x00)
			} else {
				body = append(body, 0x20, 0x01, 0x41)
				body = appendS32(body, int32(offset))
				body = append(body, 0x6a)
			}
			body = append(body, 0x36, 0x02)
			body = appendU32(body, pointerOffset+uint32(i*4))
		}
		body = append(body, 0x41)
		body = appendS32(body, int32(len(argv)))
		body = append(body, 0x20, 0x01, 0x41)
		body = appendS32(body, int32(pointerOffset))
		body = append(body, 0x6a)
	} else if len(params) == 2 {
		body = append(body, 0x41, 0x00, 0x41, 0x00)
	}
	body = append(body, 0x10)
	body = appendU32(body, target)
	if len(results) == 1 {
		if useArgv {
			body = append(body, 0x21, 0x02, 0x20, 0x00, 0x10)
			body = appendU32(body, stackRestore)
			body = append(body, 0x20, 0x02)
		}
		body = append(body, 0x04, 0x40, 0x00, 0x0b) // trap rather than silently accept a non-zero main result.
	}
	body = append(body, 0x0b)
	code := appendU32(nil, uint32(len(body)))
	code = append(code, body...)
	if _, err := appendVectorItem(sections, 10, code); err != nil {
		return nil, err
	}
	return sections, nil
}

func encodeEmscriptenArgv(args, environment []string) (data []byte, offsets []uint32, pointerOffset, allocationSize uint32, err error) {
	appendStrings := func(values []string) error {
		for _, arg := range values {
			if bytes.IndexByte([]byte(arg), 0) >= 0 {
				return fmt.Errorf("Emscripten argv or environment contains NUL")
			}
			offsets = append(offsets, uint32(len(data)))
			data = append(data, arg...)
			data = append(data, 0)
		}
		return nil
	}
	if err := appendStrings(args); err != nil {
		return nil, nil, 0, 0, err
	}
	offsets = append(offsets, ^uint32(0))
	if err := appendStrings(environment); err != nil {
		return nil, nil, 0, 0, err
	}
	offsets = append(offsets, ^uint32(0))
	for len(data)%4 != 0 {
		data = append(data, 0)
	}
	pointerOffset = uint32(len(data))
	allocationSize = pointerOffset + uint32(len(offsets)*4)
	allocationSize = allocationSize + 15&^15
	if allocationSize > 8192 {
		return nil, nil, 0, 0, fmt.Errorf("Emscripten argv exceeds the 8 KiB bootstrap area")
	}
	return data, offsets, pointerOffset, allocationSize, nil
}

func functionTypeForIndex(sections []rawSection, importedFunctions, functionIndex uint32) ([]byte, []byte, error) {
	if functionIndex < importedFunctions {
		return nil, nil, fmt.Errorf("main export refers to imported function %d", functionIndex)
	}
	index := sectionIndex(sections, 3)
	if index < 0 {
		return nil, nil, fmt.Errorf("missing function section")
	}
	payload := sections[index].payload
	count, n, err := readU32(payload)
	if err != nil {
		return nil, nil, err
	}
	wanted := functionIndex - importedFunctions
	if wanted >= count {
		return nil, nil, fmt.Errorf("function index %d exceeds function section", functionIndex)
	}
	offset := n
	for i := uint32(0); i <= wanted; i++ {
		typeIndex, size, err := readU32(payload[offset:])
		if err != nil {
			return nil, nil, err
		}
		offset += size
		if i == wanted {
			return functionType(sections, typeIndex)
		}
	}
	return nil, nil, fmt.Errorf("function index %d not found", functionIndex)
}

func addStartLauncher(sections []rawSection, importedFunctions uint32, targets, argv []string) ([]rawSection, error) {
	exports, hasStart, err := exportedFunctions(sections)
	if err != nil || hasStart {
		return sections, err
	}
	callIndexes := make([]uint32, len(targets))
	for i, target := range targets {
		index, ok := exports[target]
		if !ok {
			return nil, fmt.Errorf("missing export %q", target)
		}
		callIndexes[i] = index
	}
	typeIndex, err := appendVectorItem(sections, 1, []byte{0x60, 0x00, 0x00})
	if err != nil {
		return nil, err
	}
	definedFunctions, err := appendVectorItem(sections, 3, appendU32(nil, typeIndex))
	if err != nil {
		return nil, err
	}
	launcherIndex := importedFunctions + definedFunctions
	exportEntry := appendName(nil, "_start")
	exportEntry = append(exportEntry, 0x00)
	exportEntry = appendU32(exportEntry, launcherIndex)
	if _, err := appendVectorItem(sections, 7, exportEntry); err != nil {
		return nil, err
	}
	body := []byte{0x00}
	if len(argv) != 0 {
		data, argc, argvAddress, err := encodeGoArgv(argv)
		if err != nil {
			return nil, err
		}
		body = append(body, 0x41)
		body = appendS32(body, int32(argc))
		body = append(body, 0x41)
		body = appendS32(body, int32(argvAddress))
		segment := []byte{0x00, 0x41}
		segment = appendS32(segment, 4096)
		segment = append(segment, 0x0b)
		segment = appendU32(segment, uint32(len(data)))
		segment = append(segment, data...)
		if sectionIndex(sections, 11) < 0 {
			if err := insertStandardSection(sections, rawSection{id: 11, payload: append(appendU32(nil, 1), segment...)}); err != nil {
				return nil, err
			}
		} else if _, err := appendVectorItem(sections, 11, segment); err != nil {
			return nil, err
		}
	}
	for _, index := range callIndexes {
		body = append(body, 0x10)
		body = appendU32(body, index)
	}
	body = append(body, 0x0b)
	code := appendU32(nil, uint32(len(body)))
	code = append(code, body...)
	if _, err := appendVectorItem(sections, 10, code); err != nil {
		return nil, err
	}
	return sections, nil
}

// appendVectorItem appends one encoded item to a section vector and returns the
// item's prior zero-based index.
func appendVectorItem(sections []rawSection, id byte, item []byte) (uint32, error) {
	index := sectionIndex(sections, id)
	if index < 0 {
		return 0, fmt.Errorf("missing section %d", id)
	}
	count, n, err := readU32(sections[index].payload)
	if err != nil {
		return 0, err
	}
	payload := appendU32(nil, count+1)
	payload = append(payload, sections[index].payload[n:]...)
	payload = append(payload, item...)
	sections[index].payload = payload
	return count, nil
}

func insertStandardSection(sections []rawSection, section rawSection) error {
	if section.id == 0 || sectionIndex(sections, section.id) >= 0 {
		return fmt.Errorf("cannot insert section %d", section.id)
	}
	insert := len(sections)
	for i, current := range sections {
		if current.id != 0 && current.id > section.id {
			insert = i
			break
		}
	}
	sections = append(sections, rawSection{})
	copy(sections[insert+1:], sections[insert:])
	sections[insert] = section
	return nil
}

func exportedFunctions(sections []rawSection) (map[string]uint32, bool, error) {
	index := sectionIndex(sections, 7)
	if index < 0 {
		return nil, false, fmt.Errorf("missing export section")
	}
	payload := sections[index].payload
	count, n, err := readU32(payload)
	if err != nil {
		return nil, false, err
	}
	offset := n
	functions := make(map[string]uint32)
	hasStart := false
	for i := uint32(0); i < count; i++ {
		name, next, err := readName(payload, offset)
		if err != nil {
			return nil, false, err
		}
		if next >= len(payload) {
			return nil, false, fmt.Errorf("truncated export")
		}
		kind := payload[next]
		item, size, err := readU32(payload[next+1:])
		if err != nil {
			return nil, false, err
		}
		if kind == 0 {
			functions[name] = item
		}
		if name == "_start" {
			hasStart = true
		}
		offset = next + 1 + size
	}
	if offset != len(payload) {
		return nil, false, fmt.Errorf("trailing export bytes")
	}
	return functions, hasStart, nil
}

func encodeGoArgv(args []string) ([]byte, int, uint32, error) {
	data := make([]byte, 0, 256)
	pointers := make([]uint32, 0, len(args)+2)
	for _, arg := range args {
		if bytes.IndexByte([]byte(arg), 0) >= 0 {
			return nil, 0, 0, fmt.Errorf("Go argv contains NUL")
		}
		pointers = append(pointers, 4096+uint32(len(data)))
		data = append(data, arg...)
		data = append(data, 0)
		for len(data)%8 != 0 {
			data = append(data, 0)
		}
	}
	argv := 4096 + uint32(len(data))
	pointers = append(pointers, 0, 0) // argv terminator, then empty environment terminator.
	for _, pointer := range pointers {
		var encoded [8]byte
		binary.LittleEndian.PutUint32(encoded[:4], pointer)
		data = append(data, encoded[:]...)
	}
	if len(data) >= 8192 {
		return nil, 0, 0, fmt.Errorf("Go argv exceeds the 8 KiB bootstrap area")
	}
	return data, len(args), argv, nil
}

func sectionIndex(sections []rawSection, id byte) int {
	for i := range sections {
		if sections[i].id == id {
			return i
		}
	}
	return -1
}

func importDescriptorEnd(payload []byte, offset int, kind byte) (int, error) {
	switch kind {
	case 0:
		_, n, err := readU32(payload[offset:])
		return offset + n, err
	case 1:
		if offset >= len(payload) {
			return 0, fmt.Errorf("truncated table type")
		}
		return limitsEnd(payload, offset+1, false)
	case 2:
		return limitsEnd(payload, offset, true)
	case 3:
		if offset+2 > len(payload) {
			return 0, fmt.Errorf("truncated global type")
		}
		return offset + 2, nil
	case 4:
		_, n1, err := readU32(payload[offset:])
		if err != nil {
			return 0, err
		}
		_, n2, err := readU32(payload[offset+n1:])
		return offset + n1 + n2, err
	default:
		return 0, fmt.Errorf("unknown import kind %d", kind)
	}
}

func limitsEnd(payload []byte, offset int, memory bool) (int, error) {
	flags, n, err := readU32(payload[offset:])
	if err != nil {
		return 0, err
	}
	offset += n
	is64 := memory && flags&4 != 0
	if is64 {
		_, n, err = readU64(payload[offset:])
	} else {
		_, n, err = readU32(payload[offset:])
	}
	if err != nil {
		return 0, err
	}
	offset += n
	if flags&1 != 0 {
		if is64 {
			_, n, err = readU64(payload[offset:])
		} else {
			_, n, err = readU32(payload[offset:])
		}
		if err != nil {
			return 0, err
		}
		offset += n
	}
	return offset, nil
}

func readName(payload []byte, offset int) (string, int, error) {
	if offset > len(payload) {
		return "", 0, fmt.Errorf("truncated name")
	}
	size, n, err := readU32(payload[offset:])
	if err != nil {
		return "", 0, err
	}
	start := offset + n
	end := uint64(start) + uint64(size)
	if end > uint64(len(payload)) {
		return "", 0, fmt.Errorf("name exceeds section")
	}
	return string(payload[start:int(end)]), int(end), nil
}

func appendName(out []byte, value string) []byte {
	out = appendU32(out, uint32(len(value)))
	return append(out, value...)
}

func readU32(data []byte) (uint32, int, error) {
	value, shift := uint32(0), uint(0)
	for i, b := range data {
		if i == 5 || shift == 28 && b&0xf0 != 0 {
			return 0, 0, fmt.Errorf("invalid u32 LEB128")
		}
		value |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("truncated u32 LEB128")
}

func readU64(data []byte) (uint64, int, error) {
	value, shift := uint64(0), uint(0)
	for i, b := range data {
		if i == 10 || shift == 63 && b&0xfe != 0 {
			return 0, 0, fmt.Errorf("invalid u64 LEB128")
		}
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("truncated u64 LEB128")
}

func appendU32(out []byte, value uint32) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}

func appendS32(out []byte, value int32) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		done := value == 0 && b&0x40 == 0 || value == -1 && b&0x40 != 0
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}
