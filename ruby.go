package emscripten

import (
	"fmt"

	"github.com/wago-org/wago"
)

func (p *plugin) registerRuby(imports *wago.HostImportRegistrar) error {
	i32, f64 := wago.ValI32, wago.ValF64
	i32s := func(n int) []wago.ValType {
		out := make([]wago.ValType, n)
		for i := range out {
			out[i] = i32
		}
		return out
	}
	noop := wago.HostFunc(func(wago.HostModule, []uint64, []uint64) {})
	zero := wago.HostFunc(func(_ wago.HostModule, _ []uint64, results []uint64) {
		if len(results) != 0 {
			results[0] = 0
		}
	})

	canonical, err := imports.Module("canonical_abi")
	if err != nil {
		return err
	}
	canonical.Func("resource_drop_js-abi-value", noop).Params(i32).Docs("drop a barebones JS value handle")
	canonical.Func("resource_new_rb-abi-value", zero).Params(i32).Results(i32).Docs("wrap a Ruby ABI handle")
	canonical.Func("resource_get_rb-abi-value", zero).Params(i32).Results(i32).Docs("unwrap a Ruby ABI handle")

	ruby, err := imports.Module("rb-js-abi-host")
	if err != nil {
		return err
	}
	bindings := []hostBinding{
		{"rb_wasm_throw_prohibit_rewind_exception", func(_ wago.HostModule, _ []uint64, _ []uint64) {
			panic(wago.HostTrap{Err: fmt.Errorf("ruby: prohibited rewind crossed the host boundary")})
		}, i32s(2), nil, "trap prohibited Ruby stack rewinds"},
		{"eval-js: func(code: string) -> variant { success(handle<js-abi-value>), failure(handle<js-abi-value>) }", noop, i32s(3), nil, "return an empty failure variant for JavaScript evaluation"},
		{"is-js: func(value: handle<js-abi-value>) -> bool", zero, i32s(1), []wago.ValType{i32}, "report that bare handles are not JavaScript objects"},
		{"instance-of: func(value: handle<js-abi-value>, klass: handle<js-abi-value>) -> bool", zero, i32s(2), []wago.ValType{i32}, "report no JavaScript prototype relationship"},
		{"global-this: func() -> handle<js-abi-value>", zero, nil, []wago.ValType{i32}, "return the null barebones global handle"},
		{"int-to-js-number: func(value: s32) -> handle<js-abi-value>", zero, i32s(1), []wago.ValType{i32}, "return a bare number handle"},
		{"float-to-js-number: func(value: float64) -> handle<js-abi-value>", zero, []wago.ValType{f64}, []wago.ValType{i32}, "return a bare number handle"},
		{"string-to-js-string: func(value: string) -> handle<js-abi-value>", zero, i32s(2), []wago.ValType{i32}, "return a bare string handle"},
		{"bool-to-js-bool: func(value: bool) -> handle<js-abi-value>", zero, i32s(1), []wago.ValType{i32}, "return a bare boolean handle"},
		{"proc-to-js-function: func(value: u32) -> handle<js-abi-value>", zero, i32s(1), []wago.ValType{i32}, "return a bare function handle"},
		{"rb-object-to-js-rb-value: func(raw-rb-abi-value: u32) -> handle<js-abi-value>", zero, i32s(1), []wago.ValType{i32}, "return a bare Ruby object handle"},
		{"js-value-to-string: func(value: handle<js-abi-value>) -> string", noop, i32s(2), nil, "write an empty string result"},
		{"js-value-to-integer: func(value: handle<js-abi-value>) -> variant { as-float(float64), bignum(string) }", noop, i32s(2), nil, "write an empty integer result"},
		{"export-js-value-to-host: func(value: handle<js-abi-value>) -> ()", noop, i32s(1), nil, "ignore an exported bare JS value"},
		{"import-js-value-from-host: func() -> handle<js-abi-value>", zero, nil, []wago.ValType{i32}, "return the null imported JS value"},
		{"js-value-typeof: func(value: handle<js-abi-value>) -> string", noop, i32s(2), nil, "write an empty typeof result"},
		{"js-value-equal: func(lhs: handle<js-abi-value>, rhs: handle<js-abi-value>) -> bool", zero, i32s(2), []wago.ValType{i32}, "report unequal bare JS values"},
		{"js-value-strictly-equal: func(lhs: handle<js-abi-value>, rhs: handle<js-abi-value>) -> bool", zero, i32s(2), []wago.ValType{i32}, "report unequal bare JS values"},
		{"reflect-apply: func(target: handle<js-abi-value>, this-argument: handle<js-abi-value>, arguments: list<handle<js-abi-value>>) -> variant { success(handle<js-abi-value>), failure(handle<js-abi-value>) }", noop, i32s(5), nil, "write an empty reflection failure"},
		{"reflect-get: func(target: handle<js-abi-value>, property-key: string) -> variant { success(handle<js-abi-value>), failure(handle<js-abi-value>) }", noop, i32s(4), nil, "write an empty reflection failure"},
		{"reflect-set: func(target: handle<js-abi-value>, property-key: string, value: handle<js-abi-value>) -> variant { success(handle<js-abi-value>), failure(handle<js-abi-value>) }", noop, i32s(5), nil, "write an empty reflection failure"},
	}
	for _, binding := range bindings {
		ruby.Func(binding.name, binding.fn).Params(binding.params...).Results(binding.results...).Docs(binding.docs)
	}
	return nil
}
