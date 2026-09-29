package usagepersist

import (
	"context"
	"fmt"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaPublicationOwnsOnlyBoundedSanitizedCopies(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("codex", "index", "same.json", "private-token-A")
	auth.Metadata["account_id"] = "private-account-A"
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	bindings := source.QuotaBindings(false)
	if len(bindings) != 1 {
		t.Fatal("missing source fixture")
	}
	holder := &quotaSourceHolder{source: source}
	holder.publish(1, bindings)
	before := holder.snapshot()
	if before == nil || len(before.bindings) != 1 {
		t.Fatal("missing publication")
	}
	originalSelector := before.bindings[0].SelectorHashes["account_id"]
	bindings[0].SelectorHashes["account_id"] = "mutated-private-value"
	bindings[0].Key = "mutated-key"
	if before.bindings[0].SelectorHashes["account_id"] != originalSelector || before.bindings[0].Key != "same.json" {
		t.Fatal("publication retained the source's mutable slice/map")
	}
	for _, private := range []string{"private-token-A", "private-account-A", "mutated-private-value"} {
		if strings.Contains(fmt.Sprintf("%+v", before.bindings), private) {
			t.Fatal("publication retained private source material")
		}
	}
	// Bad proof cannot enter a long-lived publication even from a buggy source.
	holder.publish(2, bindings)
	if len(holder.snapshot().bindings) != 0 {
		t.Fatal("unhashed selector entered publication")
	}
}

func TestQuotaPublicationRejectsLateOlderReadAndPostCloseCompletion(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "index", "same.json", "A")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	old := source.QuotaBindings(false)
	current, _ := manager.GetByID(auth.ID)
	current.Metadata["access_token"] = "B"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	fresh := source.QuotaBindings(false)
	holder := &quotaSourceHolder{source: source}
	holder.publish(2, fresh)
	holder.publish(1, old)
	if got := holder.snapshot(); got == nil || got.bindings[0].CredentialGeneration != fresh[0].CredentialGeneration {
		t.Fatal("late earlier identity read replaced newer advisory proof")
	}
	holder.stop()
	holder.publish(3, fresh)
	if holder.snapshot() != nil || holder.published.Load() != nil {
		t.Fatal("closed source publication resurrected")
	}
}
