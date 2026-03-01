/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package injector

import (
	"testing"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
)

func TestBuildEnvoyProxyContainer_SpireEnabled_HasSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			if vm.MountPath != "/opt" {
				t.Errorf("svid-output mount path = %q, want /opt", vm.MountPath)
			}
			if !vm.ReadOnly {
				t.Error("svid-output mount should be read-only")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing svid-output volume mount when SPIRE is enabled")
	}
}

func TestBuildEnvoyProxyContainer_SpireDisabled_NoSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(false)

	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			t.Error("envoy-proxy container should NOT have svid-output mount when SPIRE is disabled")
		}
	}
}

func TestBuildEnvoyProxyContainer_DefaultIncludesSvidOutput(t *testing.T) {
	// The no-arg BuildEnvoyProxyContainer defaults to SPIRE enabled
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			break
		}
	}
	if !found {
		t.Error("default BuildEnvoyProxyContainer should include svid-output mount")
	}
}

func TestBuildEnvoyProxyContainer_HasAllRequiredMounts(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	requiredMounts := map[string]string{
		"envoy-config": "/etc/envoy",
		"shared-data":  "/shared",
		"svid-output":  "/opt",
	}

	mountsByName := make(map[string]string)
	for _, vm := range container.VolumeMounts {
		mountsByName[vm.Name] = vm.MountPath
	}

	for name, expectedPath := range requiredMounts {
		path, ok := mountsByName[name]
		if !ok {
			t.Errorf("missing volume mount %q", name)
			continue
		}
		if path != expectedPath {
			t.Errorf("volume mount %q path = %q, want %q", name, path, expectedPath)
		}
	}
}

func TestBuildEnvoyProxyContainer_Name(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	if container.Name != EnvoyProxyContainerName {
		t.Errorf("container name = %q, want %q", container.Name, EnvoyProxyContainerName)
	}
}

func TestBuildProxyInitContainer_DefaultOutbound(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("", "")

	var outbound string
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			outbound = env.Value
		}
	}
	if outbound != "8080" {
		t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", outbound, "8080")
	}

	for _, env := range container.Env {
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			t.Error("INBOUND_PORTS_EXCLUDE should not be set when inboundExclude is empty")
		}
	}
}

func TestBuildProxyInitContainer_OutboundExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("443,4317", "")

	var outbound string
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			outbound = env.Value
		}
	}
	if outbound != "8080,443,4317" {
		t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", outbound, "8080,443,4317")
	}
}

func TestBuildProxyInitContainer_InboundExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("", "8443,18789")

	var inbound string
	found := false
	for _, env := range container.Env {
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			inbound = env.Value
			found = true
		}
	}
	if !found {
		t.Fatal("INBOUND_PORTS_EXCLUDE env var not set")
	}
	if inbound != "8443,18789" {
		t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", inbound, "8443,18789")
	}
}

func TestBuildProxyInitContainer_BothExclusions(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("443", "8443,18789")

	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}

	if envMap["OUTBOUND_PORTS_EXCLUDE"] != "8080,443" {
		t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", envMap["OUTBOUND_PORTS_EXCLUDE"], "8080,443")
	}
	if envMap["INBOUND_PORTS_EXCLUDE"] != "8443,18789" {
		t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", envMap["INBOUND_PORTS_EXCLUDE"], "8443,18789")
	}
}
