//go:build cpex

package main

// variant marks a build that links the CPEX FFI. It goes before the version so
// that the last word of `cortex --version` is still the version.
const variant = "(cpex)"
