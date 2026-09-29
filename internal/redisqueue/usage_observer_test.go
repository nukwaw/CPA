package redisqueue

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func observerFixture(id string) coreusage.Record {
	return coreusage.Record{RequestID: id, Provider: "codex", Model: "test-model", Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}
}

func TestUsageObserverDoesNotConsumeOrMutateOriginalQueue(t *testing.T) {
	withEnabledQueue(t, func() {
		var observed []byte
		stop := ObserveUsage(func(_ context.Context, payload []byte) { observed = append([]byte(nil), payload...); payload[0] = '!' })
		defer stop()
		(&usageQueuePlugin{}).HandleUsage(context.Background(), observerFixture("observer-copy"))
		payload := popSinglePayload(t)
		requireStringField(t, payload, "execution_id", "observer-copy")
		if !json.Valid(observed) {
			t.Fatal("observer did not receive original provider JSON")
		}
		if len(PopOldest(10)) != 0 {
			t.Fatal("observer added duplicate queue entries")
		}
	})
}

func TestUsageObserverFollowsOriginalGatesWithoutNewControls(t *testing.T) {
	withEnabledQueue(t, func() {
		var ids []string
		stop := ObserveUsage(func(_ context.Context, payload []byte) {
			var event struct {
				ID string `json:"execution_id"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Error(err)
				return
			}
			ids = append(ids, event.ID)
		})
		defer stop()
		plugin := &usageQueuePlugin{}
		SetUsageStatisticsEnabled(false)
		plugin.HandleUsage(context.Background(), observerFixture("collection-off"))
		SetUsageStatisticsEnabled(true)
		SetEnabled(false)
		plugin.HandleUsage(context.Background(), observerFixture("provider-off"))
		SetEnabled(true)
		plugin.HandleUsage(context.Background(), observerFixture("resumed"))
		if len(ids) != 1 || ids[0] != "resumed" {
			t.Fatalf("original gates bypassed: %v", ids)
		}
		payload := popSinglePayload(t)
		requireStringField(t, payload, "execution_id", "resumed")
	})
}

func TestUsageObserverDoesNotChangeExistingSubscriptionMode(t *testing.T) {
	withEnabledQueue(t, func() {
		stream, unsubscribeStream := SubscribeUsage()
		defer unsubscribeStream()
		<-stream // Existing capability notification.
		var called bool
		stop := ObserveUsage(func(context.Context, []byte) { called = true })
		defer stop()
		(&usageQueuePlugin{}).HandleUsage(context.Background(), observerFixture("subscriber-and-observer"))
		var event map[string]json.RawMessage
		if err := json.Unmarshal(<-stream, &event); err != nil {
			t.Fatal(err)
		}
		requireStringField(t, event, "execution_id", "subscriber-and-observer")
		if !called || len(PopOldest(10)) != 0 {
			t.Fatal("passive observer changed existing subscription behavior")
		}
	})
}

func TestUsageObserverUnsubscribeWaitsForItsCallbacks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	stop := ObserveUsage(func(context.Context, []byte) { close(entered); <-release; finished.Store(true) })
	published := make(chan struct{})
	go func() { observeUsage(context.Background(), []byte(`{}`)); close(published) }()
	<-entered
	stopped := make(chan bool, 1)
	go func() { stop(); stopped <- finished.Load() }()
	close(release)
	if !<-stopped {
		t.Fatal("unsubscribe returned before its callback finished")
	}
	<-published
	observeUsage(context.Background(), []byte(`{}`)) // Must not invoke closed callback again.
	stop()                                           // Idempotent.
}

func TestUsageObserversRunWithoutRegistryLockAndIsolatePanics(t *testing.T) {
	withEnabledQueue(t, func() {
		stop := ObserveUsage(func(context.Context, []byte) {
			nestedStop := ObserveUsage(func(context.Context, []byte) {})
			nestedStop()
			panic("private-payload-must-not-be-logged")
		})
		defer stop()
		(&usageQueuePlugin{}).HandleUsage(context.Background(), observerFixture("panic-isolated"))
		payload := popSinglePayload(t)
		requireStringField(t, payload, "execution_id", "panic-isolated")
	})
}

func TestUsageObserverBlockedCallbackDoesNotHoldRegistryLocks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	stop := ObserveUsage(func(context.Context, []byte) { close(entered); <-release })
	published := make(chan struct{})
	go func() { observeUsage(context.Background(), []byte(`{}`)); close(published) }()
	<-entered
	registryDone := make(chan struct{})
	go func() {
		otherStop := ObserveUsage(func(context.Context, []byte) {})
		otherStop()
		close(registryDone)
	}()
	select {
	case <-registryDone:
	case <-time.After(5 * time.Second):
		close(release)
		stop()
		t.Fatal("callback held registry lock during delivery")
	}
	close(release)
	<-published
	stop()
}

func TestUsageObserverConcurrentSubscribePublishAndClose(t *testing.T) {
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 40 {
				stop := ObserveUsage(func(context.Context, []byte) {})
				observeUsage(context.Background(), []byte(`{}`))
				stop()
			}
		}()
	}
	workers.Wait()
}
