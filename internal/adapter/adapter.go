// Package adapter is the worker adapter boundary (iteration 04): immutable
// adapter metadata (ID, allowed efforts, test-only mark) available to the
// plane and coordinators without touching any filesystem, and an
// invocability probe run by the sidecar on the worker. Iteration 05 adds
// the task boundary: explicit model and effort argv with the prompt on
// stdin, incremental final-message extraction, and the native signal
// names a failed task reports. The fake adapter is a test/demo adapter
// that never calls a model. Iteration 08 adds the production Claude and
// Codex adapters with their captured recipes. Iteration 11 adds Grok
// (prompt in its -p argument, Linux execution only) and Cursor (registered
// and version-probed, execution refused on every OS), with
// ValidateWorkerPosture deciding execution eligibility separately from
// selection and version. Design 12a-worker-selection makes selection
// requirement Q12's: an explicit free-text model and an effort from the
// adapter's union, the vendor deciding whether the pair runs, and worker
// versions eligible at or above their observed minimum; the captured
// versions and pairs remain evidence. Every adapter is enabled only by an
// explicit absolute executable path on the worker.
//
// The package imports neither plane, sidecar, devcheck nor testkit. Its
// process and clock dependencies are private and injectable for package
// tests only; there is no package-init registration, plugin loading,
// environment discovery or PATH lookup.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Descriptor is an adapter's immutable metadata.
type Descriptor struct {
	ID       string
	Efforts  []string
	TestOnly bool
}

func (d Descriptor) clone() Descriptor {
	d.Efforts = append([]string(nil), d.Efforts...)
	return d
}

// Adapter is one registered adapter: its metadata, an invocability probe
// of an explicit absolute executable path and (iteration 05) the task
// boundary: an invocation for one task and a fresh final-message
// extractor. Invocation never opens a file or starts a process;
// executable paths stay sidecar-local.
type Adapter interface {
	Descriptor() Descriptor
	Probe(ctx context.Context, executable string) error
	Invocation(input TaskInput) (Invocation, error)
	NewFinalExtractor() FinalExtractor
}

// Registry is an immutable set of adapters, safe for concurrent use.
type Registry interface {
	// Lookup returns the adapter registered under id (case-sensitive).
	Lookup(id string) (Adapter, bool)
	// Descriptors returns copies of every descriptor, sorted by ID.
	Descriptors() []Descriptor
}

type registry struct {
	byID  map[string]Adapter
	descs []Descriptor
}

// NewRegistry builds an immutable registry from adapters. It rejects a nil
// adapter, a descriptor ID or effort outside the role slug grammar, an
// empty or duplicate effort set, and duplicate IDs.
func NewRegistry(adapters ...Adapter) (Registry, error) {
	r := &registry{byID: map[string]Adapter{}}
	for _, a := range adapters {
		if a == nil {
			return nil, errors.New("adapter: nil adapter")
		}
		d := a.Descriptor().clone()
		if !contract.ValidSlug(d.ID) {
			return nil, fmt.Errorf("adapter: invalid adapter ID %q: want a 1-63 character slug", contract.SafeText(d.ID, 64))
		}
		if _, dup := r.byID[d.ID]; dup {
			return nil, fmt.Errorf("adapter: duplicate adapter ID %q", d.ID)
		}
		if len(d.Efforts) == 0 {
			return nil, fmt.Errorf("adapter: adapter %s has no efforts", d.ID)
		}
		seen := map[string]bool{}
		for _, e := range d.Efforts {
			if !contract.ValidSlug(e) {
				return nil, fmt.Errorf("adapter: adapter %s has an invalid effort %q: want a 1-63 character slug", d.ID, contract.SafeText(e, 64))
			}
			if seen[e] {
				return nil, fmt.Errorf("adapter: adapter %s repeats effort %q", d.ID, e)
			}
			seen[e] = true
		}
		r.byID[d.ID] = a
		r.descs = append(r.descs, d)
	}
	sort.Slice(r.descs, func(i, j int) bool { return r.descs[i].ID < r.descs[j].ID })
	return r, nil
}

func (r *registry) Lookup(id string) (Adapter, bool) {
	a, ok := r.byID[id]
	return a, ok
}

func (r *registry) Descriptors() []Descriptor {
	out := make([]Descriptor, len(r.descs))
	for i, d := range r.descs {
		out[i] = d.clone()
	}
	return out
}

// ContractLookup adapts r to the contract's metadata lookup.
func ContractLookup(r Registry) contract.AdapterLookup {
	return func(id string) (contract.AdapterInfo, bool) {
		a, ok := r.Lookup(id)
		if !ok {
			return contract.AdapterInfo{}, false
		}
		d := a.Descriptor()
		return contract.AdapterInfo{Efforts: d.Efforts, TestOnly: d.TestOnly}, true
	}
}

// Builtin is the product registry: claude, codex, cursor, the fake adapter
// and grok (sorted by ID), whose probes run in the given working directory
// (the sidecar state root; the plane and coordinators, which never probe,
// pass ""). Construction performs no I/O. Registration is not execution
// eligibility (ValidateWorkerPosture).
func Builtin(dir string) Registry {
	r, err := NewRegistry(NewClaude(dir), NewCodex(dir), NewCursor(dir), NewFake(dir), NewGrok(dir))
	if err != nil {
		panic(err) // unreachable: the built-in descriptors are valid
	}
	return r
}

// Lookup is the metadata lookup of the product registry.
func Lookup() contract.AdapterLookup { return ContractLookup(Builtin("")) }
