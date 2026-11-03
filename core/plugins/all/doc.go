// Package all registers the bundled plugins for every binary that imports it.
// Each plugins_<name>.go file is gated by `//go:build include_plugin_<name>` and
// blank-imports one plugin, so a binary links exactly the set its build tags
// name. One file per plugin serves every binary: there is no per-binary copy to
// forget. This untagged file keeps the package importable by a build with no tags.
package all
