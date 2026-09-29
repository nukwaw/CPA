package redisqueue

import (
	"context"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Usage observers receive copies of the built-in provider's published payloads.
// Unlike queue subscriptions, observation never consumes or redirects queue data.
var usageObservers struct {
	sync.Mutex
	next  uint64
	items map[uint64]*usageObserver
}

type usageObserver struct {
	mu      sync.Mutex
	closed  bool
	active  sync.WaitGroup
	consume func(context.Context, []byte)
}

// ObserveUsage attaches a passive consumer after the provider's existing gates.
// Callbacks run synchronously without registry locks and must do bounded,
// nonblocking work. Consumers needing I/O must sanitize and admit to their own
// bounded worker; this generic observer must not queue raw provider payloads.
// Unsubscribe waits for this consumer's in-flight callbacks; call it outside a
// callback. It cannot flush records still pending in the upstream SDK dispatcher.
func ObserveUsage(consume func(context.Context, []byte)) func() {
	if consume == nil {
		return func() {}
	}
	observer := &usageObserver{consume: consume}
	usageObservers.Lock()
	if usageObservers.items == nil {
		usageObservers.items = make(map[uint64]*usageObserver)
	}
	usageObservers.next++
	id := usageObservers.next
	usageObservers.items[id] = observer
	usageObservers.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			usageObservers.Lock()
			delete(usageObservers.items, id)
			usageObservers.Unlock()
			observer.mu.Lock()
			observer.closed = true
			observer.mu.Unlock()
			observer.active.Wait()
		})
	}
}

func observeUsage(ctx context.Context, payload []byte) {
	usageObservers.Lock()
	observers := make([]*usageObserver, 0, len(usageObservers.items))
	for _, observer := range usageObservers.items {
		observers = append(observers, observer)
	}
	usageObservers.Unlock()
	for _, observer := range observers {
		observer.deliver(ctx, payload)
	}
}

func (observer *usageObserver) deliver(ctx context.Context, payload []byte) {
	observer.mu.Lock()
	if observer.closed {
		observer.mu.Unlock()
		return
	}
	observer.active.Add(1)
	observer.mu.Unlock()
	defer observer.active.Done()
	defer func() {
		if recover() != nil {
			// Arbitrary panic values may contain payloads or credentials.
			log.Error("usage observer callback failed")
		}
	}()
	observer.consume(ctx, append([]byte(nil), payload...))
}
