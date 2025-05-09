// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package stopwaiter

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"

	"github.com/offchainlabs/nitro/util/containers"
)

const stopDelayWarningTimeout = 30 * time.Second

type StopWaiterSafe struct {
	mutex     sync.Mutex // protects started, stopped, ctx, parentCtx, stopFunc
	started   bool
	stopped   bool
	ctx       context.Context
	parentCtx context.Context
	stopFunc  func()
	name      string
	waitChan  <-chan interface{}

	wg sync.WaitGroup
}

func (s *StopWaiterSafe) Started() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.started
}

func (s *StopWaiterSafe) Stopped() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.stopped
}

func (s *StopWaiterSafe) GetContextSafe() (context.Context, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.getContext()
}

// this context is not cancelled even after someone calls Stop
func (s *StopWaiterSafe) GetParentContextSafe() (context.Context, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.getParentContext()
}

// Only call this internally with the mutex held.
func (s *StopWaiterSafe) getContext() (context.Context, error) {
	if s.started {
		return s.ctx, nil
	}
	return nil, errors.New("not started")
}

// Only call this internally with the mutex held.
func (s *StopWaiterSafe) getParentContext() (context.Context, error) {
	if s.started {
		return s.parentCtx, nil
	}
	return nil, errors.New("not started")
}

func getParentName(parent any) string {
	// remove asterisk in case the type is a pointer
	return strings.Replace(reflect.TypeOf(parent).String(), "*", "", 1)
}

// start-after-start will error, start-after-stop will immediately cancel
func (s *StopWaiterSafe) Start(ctx context.Context, parent any) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.started {
		return errors.New("start after start")
	}
	s.started = true
	s.name = getParentName(parent)
	s.parentCtx = ctx
	s.ctx, s.stopFunc = context.WithCancel(s.parentCtx)
	if s.stopped {
		s.stopFunc()
	}
	return nil
}

func (s *StopWaiterSafe) StopOnly() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.started && !s.stopped {
		s.stopFunc()
	}
	s.stopped = true
}

// StopAndWait may be called multiple times, even before start.
func (s *StopWaiterSafe) StopAndWait() error {
	return s.stopAndWaitImpl(stopDelayWarningTimeout)
}

func getAllStackTraces() string {
	buf := make([]byte, 64*1024*1024)
	size := runtime.Stack(buf, true)
	builder := strings.Builder{}
	builder.Write(buf[0:size])
	return builder.String()
}

func (s *StopWaiterSafe) stopAndWaitImpl(warningTimeout time.Duration) error {
	s.StopOnly()
	if !s.Started() {
		// No need to wait, because nothing can be started if it's already stopped.
		return nil
	}
	// Even if StopOnly has been previously called, make sure we wait for everything to shut down.
	// Otherwise, a StopOnly call followed by StopAndWait might return early without waiting.
	// At this point started must be true (because it was true above and cannot go back to false),
	// so GetWaitChannel won't return an error.
	waitChan, err := s.GetWaitChannel()
	if err != nil {
		return err
	}
	timer := time.NewTimer(warningTimeout)

	select {
	case <-timer.C:
		traces := getAllStackTraces()
		log.Warn("taking too long to stop", "name", s.name, "delay[s]", warningTimeout.Seconds())
		log.Warn(traces)
	case <-waitChan:
		timer.Stop()
		return nil
	}
	<-waitChan
	return nil
}

func (s *StopWaiterSafe) GetWaitChannel() (<-chan interface{}, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.waitChan == nil {
		ctx, err := s.getContext()
		if err != nil {
			return nil, err
		}
		waitChan := make(chan interface{})
		go func() {
			<-ctx.Done()
			s.wg.Wait()
			close(waitChan)
		}()
		s.waitChan = waitChan
	}
	return s.waitChan, nil
}

// If stop was already called, thread might silently not be launched
func (s *StopWaiterSafe) LaunchThreadSafe(foo func(context.Context)) error {
	ctx, err := s.GetContextSafe()
	if err != nil {
		return err
	}
	if s.Stopped() {
		return nil
	}
	s.wg.Add(1)
	go func() {
		foo(ctx)
		s.wg.Done()
	}()
	return nil
}

// This calls go foo() directly, with the benefit of being easily searchable.
// Callers may rely on the assumption that foo runs even if this is stopped.
func (s *StopWaiterSafe) LaunchUntrackedThread(foo func()) {
	go foo()
}

// CallIteratively calls function iteratively in a thread.
// input param return value is how long to wait before next invocation
func (s *StopWaiterSafe) CallIterativelySafe(foo func(context.Context) time.Duration) error {
	return s.LaunchThreadSafe(func(ctx context.Context) {
		for {
			interval := foo(ctx)
			if ctx.Err() != nil {
				return
			}
			if interval == time.Duration(0) {
				continue
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
}

type ThreadLauncher interface {
	GetContextSafe() (context.Context, error)
	LaunchThreadSafe(foo func(context.Context)) error
	LaunchUntrackedThread(foo func())
	Stopped() bool
}

// CallIterativelyWith calls function iteratively in a thread.
// The return value of foo is how long to wait before next invocation
// Anything sent to triggerChan parameter triggers call to happen immediately
func CallIterativelyWith[T any](
	s ThreadLauncher,
	foo func(context.Context, T) time.Duration,
	triggerChan <-chan T,
) error {
	return s.LaunchThreadSafe(func(ctx context.Context) {
		var defaultVal T
		var val T
		var ok bool
		for {
			interval := foo(ctx, val)
			if ctx.Err() != nil {
				return
			}
			val = defaultVal
			if interval == time.Duration(0) {
				continue
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case val, ok = <-triggerChan:
				if !ok {
					return
				}
			}
		}
	})
}

func CallWhenTriggeredWith[T any](
	s ThreadLauncher,
	foo func(context.Context, T),
	triggerChan <-chan T,
) error {
	return s.LaunchThreadSafe(func(ctx context.Context) {
		for {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case val := <-triggerChan:
				foo(ctx, val)
			}
		}
	})
}

func LaunchPromiseThread[T any](
	s ThreadLauncher,
	foo func(context.Context) (T, error),
) containers.PromiseInterface[T] {
	ctx, err := s.GetContextSafe()
	if err != nil {
		promise := containers.NewPromise[T](nil)
		promise.ProduceError(err)
		return &promise
	}
	if s.Stopped() {
		promise := containers.NewPromise[T](nil)
		promise.ProduceError(errors.New("stopped"))
		return &promise
	}
	innerCtx, cancel := context.WithCancel(ctx)
	promise := containers.NewPromise[T](cancel)
	err = s.LaunchThreadSafe(func(context.Context) { // we don't use the param's context
		val, err := foo(innerCtx)
		if err != nil {
			promise.ProduceError(err)
		} else {
			promise.Produce(val)
		}
		cancel()
	})
	if err != nil {
		promise.ProduceError(err)
	}
	return &promise
}

func ChanRateLimiter[T any](s *StopWaiterSafe, inChan <-chan T, maxRateCallback func() time.Duration) (<-chan T, error) {
	outChan := make(chan T)
	err := s.LaunchThreadSafe(func(ctx context.Context) {
		nextAllowedTriggerTime := time.Now()
		for {
			select {
			case <-ctx.Done():
				close(outChan)
				return
			case data := <-inChan:
				now := time.Now()
				if now.After(nextAllowedTriggerTime) {
					outChan <- data
					nextAllowedTriggerTime = now.Add(maxRateCallback())
				}
			}
		}
	})
	if err != nil {
		close(outChan)
		return nil, err
	}

	return outChan, nil
}

// StopWaiter may panic on race conditions instead of returning errors
type StopWaiter struct {
	StopWaiterSafe
}

func (s *StopWaiter) Start(ctx context.Context, parent any) {
	if err := s.StopWaiterSafe.Start(ctx, parent); err != nil {
		panic(err)
	}
}

func (s *StopWaiter) StopAndWait() {
	if err := s.StopWaiterSafe.StopAndWait(); err != nil {
		panic(err)
	}
}

// If stop was already called, thread might silently not be launched
func (s *StopWaiter) LaunchThread(foo func(context.Context)) {
	if err := s.StopWaiterSafe.LaunchThreadSafe(foo); err != nil {
		panic(err)
	}
}

func (s *StopWaiter) CallIteratively(foo func(context.Context) time.Duration) {
	if err := s.StopWaiterSafe.CallIterativelySafe(foo); err != nil {
		panic(err)
	}
}

func (s *StopWaiter) GetContext() context.Context {
	ctx, err := s.StopWaiterSafe.GetContextSafe()
	if err != nil {
		panic(err)
	}
	return ctx
}

func (s *StopWaiter) GetParentContext() context.Context {
	ctx, err := s.StopWaiterSafe.GetParentContextSafe()
	if err != nil {
		panic(err)
	}
	return ctx
}
