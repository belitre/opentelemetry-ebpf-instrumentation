// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ebpf // import "go.opentelemetry.io/obi/pkg/internal/netolly/ebpf"

// Context for future maintainers: this works around kernels without d3bec0138bfb ("bpf: zero-fill
// re-used per-cpu map element", Linux 5.10, also in 5.4 and 5.9 stable releases and RHEL 8.9+). It can
// be removed once OBI's minimum supported kernel is 5.10.

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

//go:generate $BPF2GO -cc $BPF_CLANG -cflags $BPF_CFLAGS -target $BPF_TARGETS BpfSlotsProbe ../../../../bpf/netolly/slots_probe.c -- -I../../../../bpf

const slotsProbeMarker = uint64(0xdead)

// zeroFilledFlowSlots makes a new entry of the flows map start zeroed on every CPU other than the one
// that inserted it. A per-CPU map that is not preallocated allocates zeroed values for each new entry,
// while a preallocated one (LRU maps always are) reuses deleted elements, whose other-CPU values some
// kernels keep (see reusedSlotsStale).
func zeroFilledFlowSlots(spec *ebpf.CollectionSpec, staleSlots bool) {
	if !staleSlots {
		return
	}
	flows := spec.Maps[aggregatedFlowsMap]
	flows.Type = ebpf.PerCPUHash
	flows.Flags |= unix.BPF_F_NO_PREALLOC
}

// reusedSlotsStale reports whether this kernel keeps the other CPUs' values of a reused per-CPU map
// element when a BPF program inserts it. Linux 5.10 zero-fills them (d3bec0138bfb) and many older
// kernels have the fix backported, so it is probed instead of derived from the kernel version.
var reusedSlotsStale = sync.OnceValue(func() bool {
	stale, err := probeReusedSlotsStale()
	if err != nil {
		tlog().Warn("can't check whether the kernel zero-fills reused per-CPU map values. "+
			"Assuming it doesn't", "error", err)
		return true
	}
	if stale {
		tlog().Info("the kernel keeps stale per-CPU values in reused map elements " +
			"(no Linux d3bec0138bfb). The flows map won't be preallocated")
	}
	return stale
})

// probeReusedSlotsStale fills every CPU's value of all the elements of an LRU per-CPU map, deletes them,
// has a BPF program (bpf/netolly/slots_probe.c) insert new entries into the freed elements and checks
// the other CPUs' values.
func probeReusedSlotsStale() (bool, error) {
	ncpu, err := ebpf.PossibleCPU()
	if err != nil {
		return false, err
	}
	if ncpu < 2 {
		return false, nil
	}
	spec, err := LoadBpfSlotsProbe()
	if err != nil {
		return false, fmt.Errorf("loading probe spec: %w", err)
	}
	var objs BpfSlotsProbeObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return false, fmt.Errorf("loading probe: %w", err)
	}
	defer objs.Close()
	m, prog := objs.SlotsProbe, objs.ObiSlotsProbe
	entries := spec.Maps["slots_probe"].MaxEntries

	filled := make([]uint64, ncpu)
	for i := range filled {
		filled[i] = slotsProbeMarker
	}
	for k := uint32(1000); k < 1000+entries; k++ {
		if err := m.Put(k, filled); err != nil {
			return false, fmt.Errorf("filling probe map: %w", err)
		}
	}
	for k := uint32(1000); k < 1000+entries; k++ {
		if err := m.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return false, fmt.Errorf("emptying probe map: %w", err)
		}
	}

	// the program inserts the packet length as the key, and a packet is at least an Ethernet header
	const minLen = 14
	for i := range entries {
		if _, err := prog.Run(&ebpf.RunOptions{Data: make([]byte, minLen+i)}); err != nil {
			return false, fmt.Errorf("running probe program: %w", err)
		}
	}

	var values []uint64
	inserted := 0
	for k := uint32(minLen); k < minLen+entries; k++ {
		if err := m.Lookup(k, &values); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			return false, fmt.Errorf("reading probe map: %w", err)
		}
		inserted++
		if slices.Contains(values, slotsProbeMarker) {
			return true, nil
		}
	}
	if inserted == 0 {
		return false, errors.New("the probe program inserted nothing")
	}
	return false, nil
}
