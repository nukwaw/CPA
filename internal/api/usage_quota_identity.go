package api

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newUsageQuotaIdentitySource projects live credentials for the add-on.
//
// Identity is (provider, account), read directly from the live credential, so
// there is nothing to reconcile against a backing file: the projection is bounded,
// secret-free and cheap. Inference, Consume and original management handlers read
// a published copy and never wait on this call.
func newUsageQuotaIdentitySource(current func() *coreauth.Manager) usagepersist.QuotaIdentitySource {
	return usagepersist.NewQuotaIdentitySource(current)
}
