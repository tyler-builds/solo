# Patched copy of github.com/bytemare/ksf v0.5.0

Solo uses this module through github.com/bytemare/opaque. Upstream v0.5.0 doesn't build where `int` is
32 bits (TinyGo's WebAssembly, which the web client is built with): `argon2id/argon2.go` passes
`math.MaxUint32` to `fmt.Errorf` as an untyped constant, which overflows `int`. The one change here
converts it to `uint64`. Nothing else differs from upstream. Drop this copy, and the `replace` in
go.mod, once upstream has the fix.
