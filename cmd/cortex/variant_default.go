//go:build !cpex

package main

// variant is empty for the default build, so `cortex --version` prints
// exactly "cortex <version>".
const variant = ""
