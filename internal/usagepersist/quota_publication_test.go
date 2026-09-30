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
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
	bindings := source.QuotaBindings()
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
	if before.bindings[0].Account != auth.Metadata["email"] {
		t.Fatalf("publication lost the account fact: %+v", before.bindings[0])
	}
	// A binding whose selector is not a digest, or whose transient index is
	// missing, cannot enter a long-lived publication even from a buggy source.
	unhashed := QuotaBinding{Provider: "codex", Key: "same.json", AuthIndex: "index", Account: "index@example.invalid", AccountKind: "email", SelectorHashes: map[string]string{"account_id": "not-a-digest"}}
	holder.publish(2, []QuotaBinding{unhashed})
	if len(holder.snapshot().bindings) != 0 {
		t.Fatal("unhashed selector entered publication")
	}
	holder.publish(3, []QuotaBinding{{Provider: "codex", Key: "same.json", Account: "index@example.invalid", AccountKind: "email"}})
	if len(holder.snapshot().bindings) != 0 {
		t.Fatal("binding without a transient index entered publication")
	}
}

func TestQuotaPublicationRejectsLateOlderReadAndPostCloseCompletion(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "index", "same.json", "A")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
	old := source.QuotaBindings()
	current, _ := manager.GetByID(auth.ID)
	// A provider token refresh rotates the token for the same account: the
	// projection is unchanged, and the store only consults the account fact.
	current.Metadata["access_token"] = "B"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	fresh := source.QuotaBindings()
	if len(old) != 1 || len(fresh) != 1 || fresh[0].Account != old[0].Account {
		t.Fatalf("token rotation changed the projected account: %+v %+v", old, fresh)
	}
	holder := &quotaSourceHolder{source: source}
	holder.publish(2, fresh)
	holder.publish(1, old)
	if got := holder.snapshot(); got == nil || len(got.bindings) != 1 || got.sequence != 2 {
		t.Fatal("a late earlier identity read replaced a newer advisory proof")
	}
	holder.stop()
	holder.publish(3, fresh)
	if holder.snapshot() != nil || holder.published.Load() != nil {
		t.Fatal("closed source publication resurrected")
	}
}
