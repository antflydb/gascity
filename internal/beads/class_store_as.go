package beads

import "reflect"

// This file is the capability side of the typed class handles (typed-stores
// design §3.3, phase T1): one way to reach an optional capability through a
// handle, one way to ask whether two handles are one physical store, and the
// seam a class guard (T8) uses to vet class-write capabilities. It changes no
// existing lookup: the XFor helpers answer through a handle exactly as they
// did (TestXForThroughAHandleToday pins that), and As is new.
//
// Outside internal/beads the storeclass lint (tools/nogo/analyzers/storeclass)
// reports a type assertion on a store and an unwrap of a handle; As and the
// XFor helpers are what replace them.

// classLayer is a layer the class machinery puts above the store the router
// chose: a typed class handle, and from T8 the class guard. As and SameStore
// walk class layers and nothing else; they never peel a CachingStore, an
// emitter or a policy store.
type classLayer interface {
	unwrapClassLayer() Store
}

// classGateLayer is a class layer that vets class-write capabilities: the
// class guard (T8). For a class-write capability T, As asks the gate instead
// of looking below it: gateCapability stores the guarded adapter for *T in
// target and reports true, or reports false and the capability is hidden.
// An unguarded writer never leaks through a gate.
type classGateLayer interface {
	classLayer
	gateCapability(target any) bool
}

func (s WorkStore) unwrapClassLayer() Store    { return s.Store }
func (s GraphStore) unwrapClassLayer() Store   { return s.Store }
func (s SessionStore) unwrapClassLayer() Store { return s.Store }
func (s MailStore) unwrapClassLayer() Store    { return s.Store }
func (s OrdersStore) unwrapClassLayer() Store  { return s.Store }
func (s NudgesStore) unwrapClassLayer() Store  { return s.Store }

// Raw returns the store the handle wraps. It is the unwrap the storeclass
// lint baselines: prefer the handle, or As for a capability. A zero handle
// returns nil.
func (s WorkStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s GraphStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s SessionStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s MailStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s OrdersStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s NudgesStore) Raw() Store { return s.Store }

// classLayerMaxDepth bounds the walk, so a self-wrapping layer ends it.
const classLayerMaxDepth = 8

// As returns s's capability T, seeing through typed class handles. At each
// class layer, outermost first, it answers with the layer itself when the
// layer is a T (As[Store] keeps the handle); then it steps inside. Below the
// class layers it answers what the capability's XFor helper answers on that
// store (ConditionalWriterFor, GuardedBatcherFor, GraphApplyFor,
// MetadataCASWriterFor, AtomicConditionalCloserFor, NamespaceCensusFor,
// DepListBatchFor, GraphOnlyReadyFor, AssignmentGuardedUpdaterFor), or a
// plain assertion for any other T, including *CachingStore. So for every T,
// As[T](h) is exactly what the lookup answers on h's store.
//
// As[ConditionalWriter] is the capability, not the rollout policy: a write
// that obeys the conditional-writes mode still goes through
// ResolveConditionalWriter.
//
// A class-write capability (isClassWriteCapability) met at a class guard is
// the guard's adapter or nothing. As is nil-safe: nil, a zero handle (or a
// handle over one), or a walk deeper than classLayerMaxDepth answers false,
// before any layer is asked, so As[Store] never hands out an absent handle.
func As[T any](s Store) (T, bool) {
	var zero T
	writes := isClassWriteCapability[T]()
	for range classLayerMaxDepth {
		if s == nil {
			return zero, false
		}
		layer, isLayer := s.(classLayer)
		if !isLayer {
			return resolveCapability[T](s)
		}
		inner := layer.unwrapClassLayer()
		if physicalStore(inner) == nil {
			return zero, false // a zero handle, or one over a zero handle, is no store
		}
		if gate, ok := layer.(classGateLayer); ok && writes {
			var v T
			if gate.gateCapability(&v) {
				return v, true
			}
			return zero, false
		}
		if v, ok := s.(T); ok {
			return v, true
		}
		s = inner
	}
	return zero, false
}

// resolveCapability is T on a store below the class layers: its XFor helper
// when it has one, else a plain assertion.
func resolveCapability[T any](s Store) (T, bool) {
	var v T
	var ok bool
	switch p := any(&v).(type) {
	case *ConditionalWriter:
		*p, ok = ConditionalWriterFor(s)
	case *GuardedBatcher:
		*p, ok = GuardedBatcherFor(s)
	case *GraphApplyStore:
		*p, ok = GraphApplyFor(s)
	case *MetadataCASWriter:
		*p, ok = MetadataCASWriterFor(s)
	case *AtomicConditionalCloser:
		*p, ok = AtomicConditionalCloserFor(s)
	case *NamespaceCensus:
		*p, ok = NamespaceCensusFor(s)
	case *DependencyBatchLister:
		*p, ok = DepListBatchFor(s)
	case *GraphOnlyReadyStore:
		*p, ok = GraphOnlyReadyFor(s)
	case *AssignmentGuardedUpdater:
		*p, ok = AssignmentGuardedUpdaterFor(s)
	default:
		v, ok = s.(T)
	}
	return v, ok
}

// isClassWriteCapability reports whether T can create a bead or change a
// bead's class: what a class guard must vet (design §3.3.4, §3.7). The
// assignee-only and dependency writers and every reader cannot, and pass a
// guard untouched.
func isClassWriteCapability[T any]() bool {
	switch any((*T)(nil)).(type) {
	case *StorageCreateStore, *ForeignIDCreator, *StorageGraphApplyStore, *EphemeralGraphApplyStore,
		*AssignmentGuardedUpdater, *ConditionalWriter, *GuardedBatcher, *GraphApplyStore,
		*MetadataCASWriter, *AtomicConditionalCloser:
		return true
	}
	return false
}

// SameStore reports whether a and b are one physical store: the store below
// their class layers, so two class handles over one store are the same, and
// a handle is the same as the store it wraps. A nil or zero handle is the
// same as nothing, and a store whose dynamic type is not comparable (== on
// it would panic) is the same as nothing either.
func SameStore(a, b Store) (same bool) {
	a, b = physicalStore(a), physicalStore(b)
	if a == nil || b == nil {
		return false
	}
	ta := reflect.TypeOf(a)
	if ta != reflect.TypeOf(b) || !ta.Comparable() {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false // a comparable type holding a non-comparable interface value
		}
	}()
	return a == b
}

// physicalStore is s below its class layers, or nil.
func physicalStore(s Store) Store {
	for range classLayerMaxDepth {
		layer, ok := s.(classLayer)
		if !ok {
			return s
		}
		s = layer.unwrapClassLayer()
	}
	return nil
}
