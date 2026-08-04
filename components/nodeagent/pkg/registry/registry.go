// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

// Package registry provides the compile-time Source and Sink factory registry.
// Implementations register from explicit imports; Node Agent does not load
// runtime plugins.
package registry

import (
	"errors"
	"sync"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

type Dependencies struct {
	Config  config.Config
	Store   store.View
	State   *state.DB
	Logger  logger.Logger
	OnError func(error)
}

type SourceFactory func(Dependencies) (api.Source, error)
type SinkFactory func(Dependencies) (api.Sink, error)

var factories = struct {
	sync.RWMutex
	sources map[string]SourceFactory
	sinks   map[string]SinkFactory
}{sources: make(map[string]SourceFactory), sinks: make(map[string]SinkFactory)}

func RegisterSource(name string, factory SourceFactory) {
	if name == "" || factory == nil {
		panic("nodeagent: invalid Source factory registration")
	}
	factories.Lock()
	defer factories.Unlock()
	if _, exists := factories.sources[name]; exists {
		panic("nodeagent: duplicate Source factory " + name)
	}
	factories.sources[name] = factory
}

func RegisterSink(name string, factory SinkFactory) {
	if name == "" || factory == nil {
		panic("nodeagent: invalid Sink factory registration")
	}
	factories.Lock()
	defer factories.Unlock()
	if _, exists := factories.sinks[name]; exists {
		panic("nodeagent: duplicate Sink factory " + name)
	}
	factories.sinks[name] = factory
}

func BuildSource(name string, dependencies Dependencies) (api.Source, error) {
	if dependencies.State == nil || dependencies.Store == nil || dependencies.Logger == nil {
		return nil, errors.New("Source dependencies require State, Store, and Logger")
	}
	factories.RLock()
	factory := factories.sources[name]
	factories.RUnlock()
	if factory == nil {
		return nil, errors.New("Source is not compiled into Node Agent: " + name)
	}
	return factory(dependencies)
}

func BuildSink(name string, dependencies Dependencies) (api.Sink, error) {
	if dependencies.State == nil {
		return nil, errors.New("Sink dependencies require State")
	}
	factories.RLock()
	factory := factories.sinks[name]
	factories.RUnlock()
	if factory == nil {
		return nil, errors.New("Sink is not compiled into Node Agent: " + name)
	}
	return factory(dependencies)
}
