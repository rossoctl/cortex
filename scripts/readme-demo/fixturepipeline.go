package main

// fixturepipeline serves a plugin pipeline for the Pipeline pane. The plugins are
// stand-ins: they have the real plugins' names and body capabilities, and do nothing.

import (
	"context"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// FixturePlugin is one plugin in the outbound pipeline the Pipeline pane shows.
type FixturePlugin struct {
	Name              string `yaml:"name"`
	ReadsBody         bool   `yaml:"reads_body"`
	WritesRequestBody bool   `yaml:"writes_request_body"`
}

type stubPlugin struct {
	name string
	caps pipeline.PluginCapabilities
}

func (p stubPlugin) Name() string                              { return p.name }
func (p stubPlugin) Capabilities() pipeline.PluginCapabilities { return p.caps }
func (stubPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (stubPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// pipelineOptions returns the session API options that serve fs as the outbound
// pipeline and as the catalog. None when fs is empty, as for the animated demo.
func pipelineOptions(fs []FixturePlugin) ([]sessionapi.Option, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	plugins := make([]pipeline.Plugin, 0, len(fs))
	catalog := make([]sessionapi.CatalogEntry, 0, len(fs))
	for _, f := range fs {
		plugins = append(plugins, stubPlugin{f.Name, pipeline.PluginCapabilities{
			ReadsBody: f.ReadsBody, WritesRequestBody: f.WritesRequestBody}})
		catalog = append(catalog, sessionapi.CatalogEntry{Name: f.Name, Direction: "outbound", ReadsBody: f.ReadsBody})
	}
	out, err := pipeline.New(plugins)
	if err != nil {
		return nil, err
	}
	in, err := pipeline.New(nil)
	if err != nil {
		return nil, err
	}
	return []sessionapi.Option{
		sessionapi.WithPipelines(pipeline.NewHolder(in), pipeline.NewHolder(out)),
		sessionapi.WithCatalog(func() []sessionapi.CatalogEntry { return catalog }),
	}, nil
}
