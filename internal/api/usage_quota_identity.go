package api

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The add-on alone validates backing files. Inference, Consume and original
// management handlers use memory-only snapshots and never wait for this I/O.
func newUsageQuotaIdentitySource(current func() *coreauth.Manager, currentConfig func() *config.Config) usagepersist.QuotaIdentitySource {
	return usagepersist.NewQuotaIdentitySource(current, func(auth *coreauth.Auth) bool {
		path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
		backend := strings.TrimSpace(auth.Attributes[coreauth.AttributeSourceBackend])
		if backend != "" && backend != coreauth.AuthSourceFile {
			return true
		}
		if coreauth.IsPluginVirtualAuth(auth) {
			return false
		}
		if path == "" {
			// An in-memory/config credential has no file to reconcile. File credentials
			// synthesized by core carry AttributePath; do not guess from IDs or email.
			return backend != coreauth.AuthSourceFile
		}
		if !filepath.IsAbs(path) && currentConfig != nil {
			if cfg := currentConfig(); cfg != nil {
				path = filepath.Join(cfg.AuthDir, path)
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return false
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
			return false
		}
		data, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
		if err != nil || len(data) > 256<<10 {
			return false
		}
		var metadata map[string]any
		if json.Unmarshal(data, &metadata) != nil || len(metadata) > 128 {
			return false
		}
		// This bounded map is a private disk copy. Match core's genuine legacy
		// aliases and canonical-value precedence without mutating runtime data.
		coreauth.NormalizeCredentialMetadata(metadata)
		provider, _ := metadata["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(provider), auth.Provider) {
			return false
		}
		// The file must still describe the same credential the runtime observes.
		// Runtime selector attributes are inherited by the projection, because the
		// synthesizer derives some of them (the Kimi domain/base_url pair, for
		// example) and a file cannot restate them.
		runtimeBinding, okRuntime := usagepersist.ProjectQuotaBinding(auth)
		diskBinding, okDisk := usagepersist.ProjectDiskQuotaBinding(auth, metadata)
		return okRuntime && okDisk && runtimeBinding.CredentialGeneration == diskBinding.CredentialGeneration
	})
}
