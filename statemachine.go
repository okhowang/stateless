package stateless

import (
	"container/list"
	"context"
	"fmt"
	"reflect"
	"sync"
)

// State is used to to represent the possible machine states.
type State = interface{}

// Trigger is used to represent the triggers that cause state transitions.
type Trigger = interface{}

// FiringMode enumerate the different modes used when Fire-ing a trigger.
type FiringMode uint8

const (
	// FiringQueued mode shoud be used when run-to-completion is required. This is the recommended mode.
	FiringQueued FiringMode = iota
	// FiringImmediate should be used when the queing of trigger events are not needed.
	// Care must be taken when using this mode, as there is no run-to-completion guaranteed.
	FiringImmediate
)

// Transition describes a state transition.
type Transition struct {
	Source      State
	Destination State
	Trigger     Trigger
	Arguments   []interface{}

	isInitial bool
}

// IsReentry returns true if the transition is a re-entry,
// i.e. the identity transition.
func (t *Transition) IsReentry() bool {
	return t.Source == t.Destination
}

type TransitionFunc = func(context.Context, Transition)

// UnhandledTriggerActionFunc defines a function that will be called when a trigger is not handled.
type UnhandledTriggerActionFunc = func(ctx context.Context, state State, trigger Trigger, unmetGuards []string) error

// DefaultUnhandledTriggerAction is the default unhandled trigger action.
func DefaultUnhandledTriggerAction(_ context.Context, state State, trigger Trigger, unmetGuards []string) error {
	if len(unmetGuards) != 0 {
		return fmt.Errorf("stateless: Trigger '%s' is valid for transition from state '%s' but a guard conditions are not met. Guard descriptions: '%v", trigger, state, unmetGuards)
	}
	return fmt.Errorf("stateless: No valid leaving transitions are permitted from state '%s' for trigger '%s'. Consider ignoring the trigger.", state, trigger)
}

// A StateMachine is an abstract machine that can be in exactly one of a finite number of states at any given time.
// It is safe to use the StateMachine concurrently, but non of the callbacks (state manipulation, actions, events, ...) are guarded,
// so it is up to the client to protect them against race conditions.
type StateMachine struct {
	stateConfig            map[State]*stateRepresentation
	triggerConfig          map[Trigger]TriggerWithParameters
	stateAccessor          func(context.Context) (State, error)
	stateMutator           func(context.Context, State) error
	unhandledTriggerAction UnhandledTriggerActionFunc
	onTransitioningEvents  onTransitionEvents
	onTransitionedEvents   onTransitionEvents
	eventQueue             *list.List
	firingMode             FiringMode
	firing                 bool
	firingMutex            sync.Mutex
}

func newStateMachine() *StateMachine {
	return &StateMachine{
		stateConfig:            make(map[State]*stateRepresentation),
		triggerConfig:          make(map[Trigger]TriggerWithParameters),
		unhandledTriggerAction: UnhandledTriggerActionFunc(DefaultUnhandledTriggerAction),
		eventQueue:             list.New(),
	}
}

// NewStateMachine returns a queued state machine.
func NewStateMachine(initialState State) *StateMachine {
	return NewStateMachineWithMode(initialState, FiringQueued)
}

// NewStateMachineWithMode returns a state machine with the desired firing mode
func NewStateMachineWithMode(initialState State, firingMode FiringMode) *StateMachine {
	var stateMutex sync.Mutex
	sm := newStateMachine()
	reference := &stateReference{State: initialState}
	sm.stateAccessor = func(_ context.Context) (State, error) {
		stateMutex.Lock()
		defer stateMutex.Unlock()
		return reference.State, nil
	}
	sm.stateMutator = func(_ context.Context, state State) error {
		stateMutex.Lock()
		defer stateMutex.Unlock()
		reference.State = state
		return nil
	}
	sm.firingMode = firingMode
	return sm
}

// NewStateMachineWithExternalStorage returns a state machine with external state storage.
func NewStateMachineWithExternalStorage(stateAccessor func(context.Context) (State, error), stateMutator func(context.Context, State) error, firingMode FiringMode) *StateMachine {
	sm := newStateMachine()
	sm.stateAccessor = stateAccessor
	sm.stateMutator = stateMutator
	sm.firingMode = firingMode
	return sm
}

// ToGraph returns the DOT representation of the state machine.
// It is not guaranteed that the returned string will be the same in different executions.
func (sm *StateMachine) ToGraph() string {
	return newGraph().FormatStateMachine(sm)
}

// State returns the current state.
func (sm *StateMachine) State(ctx context.Context) (State, error) {
	return sm.stateAccessor(ctx)
}

// MustState returns the current state without the error.
// It is safe to use this method when used together with NewStateMachine
// or when using NewStateMachineWithExternalStorage with an state accessor that
// does not return an error.
func (sm *StateMachine) MustState() State {
	st, err := sm.State(context.Background())
	if err != nil {
		panic(err)
	}
	return st
}

// PermittedTriggers see PermittedTriggersCtx.
func (sm *StateMachine) PermittedTriggers(args ...interface{}) ([]Trigger, error) {
	return sm.PermittedTriggersCtx(context.Background(), args...)
}

// PermittedTriggersCtx returns the currently-permissible trigger values.
func (sm *StateMachine) PermittedTriggersCtx(ctx context.Context, args ...interface{}) ([]Trigger, error) {
	sr, err := sm.currentState(ctx)
	if err != nil {
		return nil, err
	}
	return sr.PermittedTriggers(ctx, args...), nil
}

// Activate see ActivateCtx.
func (sm *StateMachine) Activate() error {
	return sm.ActivateCtx(context.Background())
}

// ActivateCtx activates current state. Actions associated with activating the current state will be invoked.
// The activation is idempotent and subsequent activation of the same current state
// will not lead to re-execution of activation callbacks.
func (sm *StateMachine) ActivateCtx(ctx context.Context) error {
	sr, err := sm.currentState(ctx)
	if err != nil {
		return err
	}
	return sr.Activate(ctx)
}

// Deactivate see DeactivateCtx.
func (sm *StateMachine) Deactivate() error {
	return sm.DeactivateCtx(context.Background())
}

// DeactivateCtx deactivates current state. Actions associated with deactivating the current state will be invoked.
// The deactivation is idempotent and subsequent deactivation of the same current state
// will not lead to re-execution of deactivation callbacks.
func (sm *StateMachine) DeactivateCtx(ctx context.Context) error {
	sr, err := sm.currentState(ctx)
	if err != nil {
		return err
	}
	return sr.Deactivate(ctx)
}

// IsInState see IsInStateCtx.
func (sm *StateMachine) IsInState(state State) (bool, error) {
	return sm.IsInStateCtx(context.Background(), state)
}

// IsInStateCtx determine if the state machine is in the supplied state.
// Returns true if the current state is equal to, or a substate of, the supplied state.
func (sm *StateMachine) IsInStateCtx(ctx context.Context, state State) (bool, error) {
	sr, err := sm.currentState(ctx)
	if err != nil {
		return false, err
	}
	return sr.IsIncludedInState(state), nil
}

// CanFire see CanFireCtx.
func (sm *StateMachine) CanFire(trigger Trigger, args ...interface{}) (bool, error) {
	return sm.CanFireCtx(context.Background(), trigger, args...)
}

// CanFireCtx returns true if the trigger can be fired in the current state.
func (sm *StateMachine) CanFireCtx(ctx context.Context, trigger Trigger, args ...interface{}) (bool, error) {
	sr, err := sm.currentState(ctx)
	if err != nil {
		return false, err
	}
	return sr.CanHandle(ctx, trigger, args...), nil
}

// SetTriggerParameters specify the arguments that must be supplied when a specific trigger is fired.
func (sm *StateMachine) SetTriggerParameters(trigger Trigger, argumentTypes ...reflect.Type) {
	config := TriggerWithParameters{Trigger: trigger, ArgumentTypes: argumentTypes}
	if _, ok := sm.triggerConfig[config.Trigger]; ok {
		panic(fmt.Sprintf("stateless: Parameters for the trigger '%s' have already been configured.", trigger))
	}
	sm.triggerConfig[trigger] = config
}

// Fire see FireCtx
func (sm *StateMachine) Fire(trigger Trigger, args ...interface{}) error {
	return sm.FireCtx(context.Background(), trigger, args...)
}

// FireCtx transition from the current state via the specified trigger.
// The target state is determined by the configuration of the current state.
// Actions associated with leaving the current state and entering the new one will be invoked.
//
// An error is returned if any of the state machine actions or the state callbacks return an error
// without wrapping. It can also return an error if the trigger is not mapped to any state change,
// being this error the one returned by `OnUnhandledTrigger` func.
//
// There is no rollback mechanism in case there is an action error after the state has been changed.
// Guard clauses or error states can be used gracefully handle this situations.
func (sm *StateMachine) FireCtx(ctx context.Context, trigger Trigger, args ...interface{}) error {
	return sm.internalFire(ctx, trigger, args...)
}

// OnTransitioned registers a callback that will be invoked every time the state machine
// successfully finishes a transitions from one state into another.
func (sm *StateMachine) OnTransitioned(fn ...TransitionFunc) {
	sm.onTransitionedEvents = append(sm.onTransitionedEvents, fn...)
}

// OnTransitioning registers a callback that will be invoked every time the state machine
// starts a transitions from one state into another.
func (sm *StateMachine) OnTransitioning(fn ...TransitionFunc) {
	sm.onTransitioningEvents = append(sm.onTransitioningEvents, fn...)
}

// OnUnhandledTrigger override the default behaviour of returning an error when an unhandled trigger.
func (sm *StateMachine) OnUnhandledTrigger(fn UnhandledTriggerActionFunc) {
	sm.unhandledTriggerAction = fn
}

// Configure begin configuration of the entry/exit actions and allowed transitions
// when the state machine is in a particular state.
func (sm *StateMachine) Configure(state State) *StateConfiguration {
	return &StateConfiguration{sm: sm, sr: sm.stateRepresentation(state), lookup: sm.stateRepresentation}
}

// String returns a human-readable representation of the state machine.
// It is not guaranteed that the order of the PermittedTriggers is the same in consecutive executions.
func (sm *StateMachine) String() string {
	state, err := sm.State(context.Background())
	if err != nil {
		return ""
	}

	// PermittedTriggers only returns an error if state accessor returns one, and it has already been checked.
	triggers, _ := sm.PermittedTriggers()
	return fmt.Sprintf("StateMachine {{ State = %v, PermittedTriggers = %v }}", state, triggers)
}

func (sm *StateMachine) setState(ctx context.Context, state State) error {
	return sm.stateMutator(ctx, state)
}

func (sm *StateMachine) currentState(ctx context.Context) (sr *stateRepresentation, err error) {
	var state State
	state, err = sm.State(ctx)
	if err == nil {
		sr = sm.stateRepresentation(state)
	}
	return
}

func (sm *StateMachine) stateRepresentation(state State) (sr *stateRepresentation) {
	var ok bool
	if sr, ok = sm.stateConfig[state]; !ok {
		sr = newstateRepresentation(state)
		sm.stateConfig[state] = sr
	}
	return
}

func (sm *StateMachine) internalFire(ctx context.Context, trigger Trigger, args ...interface{}) error {
	switch sm.firingMode {
	case FiringImmediate:
		return sm.internalFireOne(ctx, trigger, args...)
	case FiringQueued:
		fallthrough
	default:
		return sm.internalFireQueued(ctx, trigger, args...)
	}
}

func (sm *StateMachine) internalFireQueued(ctx context.Context, trigger Trigger, args ...interface{}) (err error) {
	sm.firingMutex.Lock()
	if sm.firing {
		sm.eventQueue.PushBack(queuedTrigger{Trigger: trigger, Args: args})
		sm.firingMutex.Unlock()
		return nil
	}
	sm.firing = true
	sm.firingMutex.Unlock()
	defer func() {
		sm.firingMutex.Lock()
		sm.firing = false
		sm.firingMutex.Unlock()
	}()
	err = sm.internalFireOne(ctx, trigger, args...)
	if err != nil {
		return
	}

	sm.firingMutex.Lock()
	e := sm.eventQueue.Front()
	sm.firingMutex.Unlock()

	for e != nil {
		et := e.Value.(queuedTrigger)
		err = sm.internalFireOne(ctx, et.Trigger, et.Args...)
		if err != nil {
			break
		}
		sm.firingMutex.Lock()
		sm.eventQueue.Remove(e)
		e = sm.eventQueue.Front()
		sm.firingMutex.Unlock()
	}
	return
}

func (sm *StateMachine) internalFireOne(ctx context.Context, trigger Trigger, args ...interface{}) (err error) {
	var (
		config TriggerWithParameters
		ok     bool
	)
	if config, ok = sm.triggerConfig[trigger]; ok {
		config.validateParameters(args...)
	}
	source, err := sm.State(ctx)
	if err != nil {
		return
	}
	representativeState := sm.stateRepresentation(source)
	var result triggerBehaviourResult
	if result, ok = representativeState.FindHandler(ctx, trigger, args...); !ok {
		return sm.unhandledTriggerAction(ctx, representativeState.State, trigger, result.UnmetGuardConditions)
	}
	switch t := result.Handler.(type) {
	case *ignoredTriggerBehaviour:
		// ignored
	case *reentryTriggerBehaviour:
		transition := Transition{Source: source, Destination: t.Destination, Trigger: trigger, Arguments: args}
		err = sm.handleReentryTrigger(ctx, representativeState, transition, args...)
	case *dynamicTriggerBehaviour:
		destination, ok := t.ResultsInTransitionFrom(ctx, source, args...)
		if !ok {
			err = fmt.Errorf("stateless: Dynamic handler for trigger %s in state %s has failed", trigger, source)
		} else {
			transition := Transition{Source: source, Destination: destination, Trigger: trigger, Arguments: args}
			err = sm.handleTransitioningTrigger(ctx, representativeState, transition, args...)
		}
	case *transitioningTriggerBehaviour:
		transition := Transition{Source: source, Destination: t.Destination, Trigger: trigger, Arguments: args}
		err = sm.handleTransitioningTrigger(ctx, representativeState, transition, args...)
	case *internalTriggerBehaviour:
		var sr *stateRepresentation
		sr, err = sm.currentState(ctx)
		if err == nil {
			transition := Transition{Source: source, Destination: source, Trigger: trigger, Arguments: args}
			err = sr.InternalAction(ctx, transition, args...)
		}
	}
	return
}

func (sm *StateMachine) handleReentryTrigger(ctx context.Context, sr *stateRepresentation, transition Transition, args ...interface{}) error {
	if err := sr.Exit(ctx, transition, args...); err != nil {
		return err
	}
	newSr := sm.stateRepresentation(transition.Destination)
	if !transition.IsReentry() {
		transition = Transition{Source: transition.Destination, Destination: transition.Destination, Trigger: transition.Trigger, Arguments: args}
		if err := newSr.Exit(ctx, transition, args...); err != nil {
			return err
		}
	}
	sm.onTransitioningEvents.Invoke(ctx, transition)
	rep, err := sm.enterState(ctx, newSr, transition, args...)
	if err != nil {
		return err
	}
	if err := sm.setState(ctx, rep.State); err != nil {
		return err
	}
	sm.onTransitionedEvents.Invoke(ctx, transition)
	return nil
}

func (sm *StateMachine) handleTransitioningTrigger(ctx context.Context, sr *stateRepresentation, transition Transition, args ...interface{}) error {
	if err := sr.Exit(ctx, transition, args...); err != nil {
		return err
	}
	sm.onTransitioningEvents.Invoke(ctx, transition)
	if err := sm.setState(ctx, transition.Destination); err != nil {
		return err
	}
	newSr := sm.stateRepresentation(transition.Destination)
	rep, err := sm.enterState(ctx, newSr, transition, args...)
	if err != nil {
		return err
	}
	// Check if state has changed by entering new state (by firing triggers in OnEntry or such)
	if rep.State != newSr.State {
		if err := sm.setState(ctx, rep.State); err != nil {
			return err
		}
	}
	sm.onTransitionedEvents.Invoke(ctx, Transition{transition.Source, rep.State, transition.Trigger, args, false})
	return nil
}

func (sm *StateMachine) enterState(ctx context.Context, sr *stateRepresentation, transition Transition, args ...interface{}) (*stateRepresentation, error) {
	// Enter the new state
	err := sr.Enter(ctx, transition, args...)
	if err != nil {
		return nil, err
	}
	// Recursively enter substates that have an initial transition
	if sr.HasInitialState {
		isValidForInitialState := false
		for _, substate := range sr.Substates {
			// Verify that the target state is a substate
			// Check if state has substate(s), and if an initial transition(s) has been set up.
			if substate.State == sr.InitialTransitionTarget {
				isValidForInitialState = true
				break
			}
		}
		if !isValidForInitialState {
			panic(fmt.Sprintf("stateless: The target (%s) for the initial transition is not a substate.", sr.InitialTransitionTarget))
		}
		initialTranslation := Transition{Source: transition.Source, Destination: sr.InitialTransitionTarget, Trigger: transition.Trigger, isInitial: true, Arguments: args}
		sr = sm.stateRepresentation(sr.InitialTransitionTarget)
		sm.onTransitioningEvents.Invoke(ctx, Transition{transition.Destination, initialTranslation.Destination, transition.Trigger, args, false})
		sr, err = sm.enterState(ctx, sr, initialTranslation, args...)
	}
	return sr, err
}
