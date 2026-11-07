package main

// The names agentop uses to find, start and stop the Cortex proxy. install.sh's
// proxy_running and pre_rename_proxy_running check the same names, so change
// both together. A core/audits rule fails on a bare "cortex" or "proxy.pid"
// literal in agentop's other source files.
const (
	// proxyBinary is the file name of the proxy in the bin directory.
	proxyBinary = "cortex"
	// preRenameBinary is the file name of the proxy in v0.7.0.
	preRenameBinary = "authbridge-proxy"
	// proxyPIDFile is the pid file of the unsupervised proxy, in the Cortex directory.
	proxyPIDFile = "proxy.pid"
)
