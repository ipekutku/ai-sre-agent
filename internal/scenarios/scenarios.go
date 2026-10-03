// Package scenarios loads reproducible incident scenario definitions.
//
// A scenario directory contains:
//
//	scenario.yaml      the incident to raise and the fault to inject (this package)
//	ground-truth.yaml  the expected diagnosis (package evaluation only)
//
// This package deliberately does not read ground-truth.yaml. The scenario
// runner passes only Scenario.Incident to the investigation agent; the fault
// definition is for the runner.
package scenarios

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/ipekutku/ai-sre-agent/internal/incident"
)

// FileName is the scenario definition file within a scenario directory.
const FileName = "scenario.yaml"

// maxFaultLatencyMS matches the cap enforced by inventory-api's fault API.
const maxFaultLatencyMS = 10_000

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Scenario is a reproducible incident definition.
type Scenario struct {
	ID          string            `yaml:"id"`
	Description string            `yaml:"description"`
	Incident    incident.Incident `yaml:"incident"`
	Fault       Fault             `yaml:"fault"`
	Trigger     Trigger           `yaml:"trigger"`
}

// Trigger is the alert condition the runner waits for before raising the
// incident: the first value of Query must exceed Above within TimeoutSeconds.
// It is for the runner only; the agent never sees it.
type Trigger struct {
	Query          string  `yaml:"query"`
	Above          float64 `yaml:"above"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
}

// Fault describes the failure the scenario runner injects.
type Fault struct {
	Service   string `yaml:"service"`
	Type      string `yaml:"type"`
	LatencyMS int    `yaml:"latency_ms"`
}

// Load reads and validates dir/scenario.yaml. The scenario ID must match the
// directory name.
func Load(dir string) (Scenario, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("read scenario: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Scenario{}, fmt.Errorf("invalid scenario %s: %w", path, err)
	}
	if base := filepath.Base(filepath.Clean(dir)); s.ID != base {
		return Scenario{}, fmt.Errorf("invalid scenario %s: id %q does not match directory %q", path, s.ID, base)
	}
	return s, nil
}

// Validate reports whether s is complete and its fault is supported.
func (s Scenario) Validate() error {
	var errs []error
	if !idPattern.MatchString(s.ID) {
		errs = append(errs, fmt.Errorf("id must be lowercase kebab-case, got %q", s.ID))
	}
	if err := s.Incident.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("incident: %w", err))
	}
	if err := s.Fault.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("fault: %w", err))
	}
	if s.Trigger.Query == "" {
		errs = append(errs, errors.New("trigger: query is required"))
	}
	if s.Trigger.TimeoutSeconds <= 0 || s.Trigger.TimeoutSeconds > 600 {
		errs = append(errs, fmt.Errorf("trigger: timeout_seconds must be in (0, 600], got %d", s.Trigger.TimeoutSeconds))
	}
	return errors.Join(errs...)
}

// Validate reports whether f is a fault the environment can inject.
// Only inventory-api latency is supported so far.
func (f Fault) Validate() error {
	if f.Service != "inventory-api" {
		return fmt.Errorf("service %q does not support fault injection", f.Service)
	}
	if f.Type != "latency" {
		return fmt.Errorf("unsupported type %q (supported: latency)", f.Type)
	}
	if f.LatencyMS <= 0 || f.LatencyMS > maxFaultLatencyMS {
		return fmt.Errorf("latency_ms must be in (0, %d], got %d", maxFaultLatencyMS, f.LatencyMS)
	}
	return nil
}
