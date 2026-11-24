// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package mute

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iUnknown             = guid("{00000000-0000-0000-C000-000000000046}")
	iSessionNotification = guid("{641DD20B-4D41-49CC-ABA3-174B9477BB08}")
	notificationSinks    sync.Map
)

// Keep a callback object pinned and rooted while Windows owns a reference.
type notificationSink struct {
	vtable   *[4]uintptr
	refs     atomic.Uint32
	pin      runtime.Pinner
	queue    *sessionNotifications
	endpoint string
}

type notifiedSession struct {
	control  *com
	endpoint string
}

// Transfer retained COM interfaces to the watchdog's MTA thread.
type sessionNotifications struct {
	mu       sync.Mutex
	closed   bool
	pending  chan notifiedSession
	wake     chan struct{}
	managers map[string]sessionRegistration
	held     map[identity]notifiedSession
}

type sessionRegistration struct {
	manager *com
	sink    *notificationSink
}

var notificationVtable = [4]uintptr{
	windows.NewCallback(func(s *notificationSink, id *windows.GUID, out **notificationSink) uintptr {
		const noInterface = 0x80004002
		if out == nil {
			return 0x80004003 // E_POINTER
		}
		*out = nil
		if id == nil || (*id != iUnknown && *id != iSessionNotification) {
			return noInterface
		}
		s.refs.Add(1)
		*out = s
		return 0
	}),
	windows.NewCallback(func(s *notificationSink) uintptr { return uintptr(s.refs.Add(1)) }),
	windows.NewCallback(releaseNotificationSink),
	windows.NewCallback(func(s *notificationSink, control *com) uintptr {
		if control != nil {
			s.queue.enqueue(control, s.endpoint)
		}
		return 0
	}),
}

// Drop the Go reference after the last COM reference is gone.
func releaseNotificationSink(s *notificationSink) uintptr {
	refs := s.refs.Add(^uint32(0))
	if refs == 0 {
		notificationSinks.Delete(s)
		s.pin.Unpin()
	}
	return uintptr(refs)
}

// Leave enumeration available if a burst fills the callback queue.
func (n *sessionNotifications) enqueue(control *com, endpoint string) {
	_ = control.invoke(1)
	n.mu.Lock()
	kept := !n.closed && len(n.pending) < cap(n.pending)
	if kept {
		n.pending <- notifiedSession{control, endpoint}
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
	n.mu.Unlock()
	if !kept {
		control.release()
	}
}

// Register each endpoint once before its first enumeration activates notifications.
func (n *sessionNotifications) register(manager *com, id string) error {
	if _, exists := n.managers[id]; n.closed || exists {
		return nil
	}
	sink := &notificationSink{vtable: &notificationVtable, queue: n, endpoint: id}
	sink.refs.Store(1)
	sink.pin.Pin(sink)
	notificationSinks.Store(sink, struct{}{})
	if err := manager.invoke(6, uintptr(unsafe.Pointer(sink))); err != nil {
		releaseNotificationSink(sink)
		return err
	}
	// The registration must retain the manager beyond this enumeration.
	_ = manager.invoke(1)
	n.managers[id] = sessionRegistration{manager, sink}
	return nil
}

// Process the exact interface from the callback even if enumeration missed it.
func (n *sessionNotifications) drain(fn func(audioSession) error) error {
	var failures error
	// Retry a held session even if subsequent enumerations omit it.
	for _, event := range n.held {
		failures = errors.Join(failures, visitSession(event.control, event.endpoint, fn))
	}
	// stop drains pending references before recovery starts.
	for range len(n.pending) {
		event := <-n.pending
		held := false
		failures = errors.Join(failures, visitSession(event.control, event.endpoint, func(session audioSession) error {
			if _, exists := n.held[session.ID]; !exists && match(session.Label) {
				n.held[session.ID] = event
				held = true
			}
			return fn(session)
		}))
		if !held {
			event.control.release()
		}
	}
	return failures
}

// Stop callbacks before recovery while retaining matching sessions until restoration.
func (n *sessionNotifications) stop() error {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	var failures error
	for _, registration := range n.managers {
		failures = errors.Join(failures, registration.manager.invoke(7, uintptr(unsafe.Pointer(registration.sink))))
		registration.manager.release()
		releaseNotificationSink(registration.sink)
	}
	for range len(n.pending) {
		event := <-n.pending
		event.control.release()
	}
	return failures
}

// Release the off-enumerator controls after recovery finishes.
func (n *sessionNotifications) releaseHeld() {
	for _, event := range n.held {
		event.control.release()
	}
}
