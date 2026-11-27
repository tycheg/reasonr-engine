package memory_test

import (
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Port conformance tests on memory adapters — the reference for the others.

func TestUnitStore(t *testing.T) {
	conformance.RunUnitStore(t, func(_ *testing.T, c kind.Catalog) conformance.Stores {
		s := memory.NewStore(c)
		return conformance.Stores{Unit: s, Timers: s, Effects: s}
	})
}

func TestRequests(t *testing.T) {
	conformance.RunRequests(t, func(_ *testing.T, c kind.Catalog) conformance.Stores {
		s := memory.NewStore(c)
		return conformance.Stores{Unit: s, Timers: s, Effects: s}
	})
}

func TestPersonal(t *testing.T) {
	conformance.RunPersonal(t, func(_ *testing.T, c kind.Catalog) conformance.Stores {
		kms := pii.NewMemoryKMS()
		s := memory.NewStoreKMS(c, kms)
		return conformance.Stores{Unit: s, Timers: s, Effects: s, Dump: s.Dump, Separate: memory.NewStore(c), KMS: kms}
	})
}

func TestProjectionStore(t *testing.T) {
	conformance.RunProjectionStore(t, func(_ *testing.T, c kind.Catalog) kind.ProjectionStore {
		return memory.NewStore(c)
	})
}

func TestHub(t *testing.T) {
	conformance.RunHub(t, func(*testing.T) kind.NotificationHub { return memory.NewHub(false) })
}

func TestCoordinator(t *testing.T) {
	conformance.RunCoordinator(t, func(*testing.T) port.Coordinator { return memory.NewCoordinator() })
}

func TestPublisher(t *testing.T) {
	conformance.RunPublisher(t, func(*testing.T) (port.Publisher, func(*testing.T, int) []conformance.Received) {
		b := &memory.Bus{}
		read := 0
		return b, func(t *testing.T, n int) []conformance.Received {
			msgs := b.Messages()
			if len(msgs) < read+n {
				t.Fatalf("published %d, want %d more", len(msgs)-read, n)
			}
			var out []conformance.Received
			for i := read; i < read+n; i++ {
				out = append(out, conformance.ReceivedOf(&msgs[i]))
			}
			read += n
			return out
		}
	})
}

func TestEntitySink(t *testing.T) {
	conformance.RunEntitySink(t, func(_ *testing.T, c kind.Catalog) conformance.Stores {
		s := memory.NewStore(c)
		return conformance.Stores{Unit: s, Timers: s, Effects: s}
	})
}

func TestStateTransfer(t *testing.T) {
	stores := func(_ *testing.T, c kind.Catalog) conformance.Stores {
		s := memory.NewStore(c)
		return conformance.Stores{Unit: s, Timers: s, Effects: s}
	}
	conformance.RunStateTransfer(t, stores, stores)
}
