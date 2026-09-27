// Package adapter is the worker adapter boundary (iteration 04): immutable
// adapter metadata (ID, allowed efforts, test-only mark) available to the
// plane and coordinators without touching any filesystem, and an
// invocability probe run by the sidecar on the worker. Only the fake
// adapter exists; it is a test/demo adapter that never calls a model and is
// enabled only by an explicit absolute executable path on the worker.
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

// Adapter is one registered adapter: its metadata and an invocability
// probe of an explicit absolute executable path.
type Adapter interface {
	Descriptor() Descriptor
	Probe(ctx context.Context, executable string) error
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

// Builtin is the product registry: exactly the fake adapter, whose probe
// runs in the given working directory (the sidecar state root; the plane
// and coordinators, which never probe, pass "").
func Builtin(dir string) Registry {
	r, err := NewRegistry(NewFake(dir))
	if err != nil {
		panic(err) // unreachable: the fake descriptor is valid
	}
	return r
}

// Lookup is the metadata lookup of the product registry.
func Lookup() contract.AdapterLookup { return ContractLookup(Builtin("")) }
