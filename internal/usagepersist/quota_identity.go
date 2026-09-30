package usagepersist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// QuotaBinding is a bounded, secret-free projection of one live credential.
//
// Durable identity is (Provider, Account). There is no credential generation, no
// revision and no token: the token rotates during ordinary operation, so it
// cannot identify an account, and a generation that changed on every rotation
// only made a healthy account look like a new credential.
//
// Account is the value of the credential's account property (see accountProperty).
// It is empty for a credential that exposes none, and such a credential is then
// grouped by provider alone rather than guessed at.
//
// AuthIndex addresses a live credential in the core manager and matches a native
// original-page request to its credential. It is transient: it is never persisted
// and never a storage key, because it changes when a credential is re-registered.
//
// SelectorHashes are digests of the operation selectors a browser-originated
// manual quota fetch must supply (codex account_id, antigravity project_id, xai
// sub/user_id). They are a precondition for trusting such a capture. They are not
// identity and never take part in grouping.
type QuotaBinding struct {
	Provider       string            `json:"provider"`
	Key            string            `json:"key"`
	Account        string            `json:"account"`
	AccountKind    string            `json:"account_kind"`
	AuthIndex      string            `json:"auth_index"`
	SelectorHashes map[string]string `json:"-"`
}

// QuotaIdentitySource must return immutable, sanitized projections. It may block
// on credential-manager locks. Only the independent worker and explicit add-on
// APIs may call a source; original handlers read a published copy instead.
type QuotaIdentitySource interface {
	QuotaBindings() []QuotaBinding
}

type quotaSourceHolder struct {
	source    QuotaIdentitySource
	reads     atomic.Uint64
	stopped   atomic.Bool
	published atomic.Pointer[quotaIdentityPublication]
}

// Published bindings are advisory request-start evidence, never write authority.
// They are bounded projection copies and can be stale indefinitely. No available
// publication means an original handler observation is skipped rather than guessed.
// There is deliberately no refresher goroutine that can become stuck on core I/O.
type quotaIdentityPublication struct {
	sequence uint64
	bindings []QuotaBinding
	index    quotaIdentityIndex
}

func (source *quotaSourceHolder) snapshot() *quotaIdentityPublication {
	if source == nil || source.stopped.Load() {
		return nil
	}
	return source.published.Load()
}

func (source *quotaSourceHolder) stop() {
	source.stopped.Store(true)
	source.published.Store(nil)
}

func (source *quotaSourceHolder) publish(sequence uint64, bindings []QuotaBinding) {
	// An invalid/unbounded source cannot leave an eligible old publication behind.
	index := indexQuotaBindings(bindings)
	copyBindings := make([]QuotaBinding, 0, len(index))
	for _, binding := range index {
		if binding.AuthIndex == "" || !boundedPublishedBinding(binding) {
			continue
		}
		binding.Provider = strings.Clone(binding.Provider)
		binding.Key = strings.Clone(binding.Key)
		binding.Account = strings.Clone(binding.Account)
		binding.AccountKind = strings.Clone(binding.AccountKind)
		binding.AuthIndex = strings.Clone(binding.AuthIndex)
		if binding.SelectorHashes != nil {
			selectors := make(map[string]string, len(binding.SelectorHashes))
			for key, value := range binding.SelectorHashes {
				selectors[strings.Clone(key)] = strings.Clone(value)
			}
			binding.SelectorHashes = selectors
		}
		copyBindings = append(copyBindings, binding)
	}
	publication := &quotaIdentityPublication{sequence: sequence, bindings: copyBindings, index: indexQuotaBindings(copyBindings)}
	for !source.stopped.Load() {
		previous := source.published.Load()
		if previous != nil && previous.sequence > sequence {
			return // A slow earlier lookup must not replace a later publication.
		}
		if source.published.CompareAndSwap(previous, publication) {
			if source.stopped.Load() {
				source.published.CompareAndSwap(publication, nil)
			}
			return
		}
	}
}

func boundedPublishedBinding(binding QuotaBinding) bool {
	if _, known := quotaCacheSchemas[binding.Provider]; !known || binding.Key == "" || len(binding.Key) > 512 || binding.AuthIndex == "" {
		return false
	}
	if !safeCacheText(strings.ReplaceAll(binding.Key, "\x00", ""), 512) || !safeCacheText(binding.AuthIndex, 256) {
		return false
	}
	if binding.Account != "" && (len(binding.Account) > 256 || !safeCacheText(binding.Account, 256)) {
		return false
	}
	if binding.AccountKind != "" && binding.AccountKind != accountProperty(binding.Provider) {
		return false
	}
	if len(binding.SelectorHashes) > 4 {
		return false
	}
	for key, hash := range binding.SelectorHashes {
		if !operationSelectorKnown(binding.Provider, key) || !validTokenHash(hash) {
			return false
		}
	}
	return true
}

const maxQuotaIdentities = 4096

// quotaIdentityIndex maps a transient credential index to its binding. Cached
// evidence is never used as mutation authority.
type quotaIdentityIndex map[string]QuotaBinding

func indexQuotaBindings(bindings []QuotaBinding) quotaIdentityIndex {
	if len(bindings) > maxQuotaIdentities {
		return nil
	}
	byIndex := make(quotaIdentityIndex, len(bindings))
	for _, binding := range bindings {
		if _, exists := byIndex[binding.AuthIndex]; exists {
			byIndex[binding.AuthIndex] = QuotaBinding{} // Duplicate indexes stay refused.
		} else {
			byIndex[binding.AuthIndex] = binding
		}
	}
	return byIndex
}

func (bindings quotaIdentityIndex) binding(provider, index string) (QuotaBinding, bool) {
	binding := bindings[strings.TrimSpace(index)]
	return binding, binding.AuthIndex != "" && (provider == "" || binding.Provider == strings.ToLower(strings.TrimSpace(provider)))
}

// BindQuotaIdentitySource binds once without querying the source. It is safe in
// the first per-request adapter call even if core currently holds a credential
// lock across storage I/O. An explicit add-on identity/read API primes advisory
// request-start evidence; originals arriving before that simply skip observation.
func (s *Store) BindQuotaIdentitySource(source QuotaIdentitySource) {
	if s == nil || source == nil || s.quotaSource.Load() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closing {
		s.quotaSource.CompareAndSwap(nil, &quotaSourceHolder{source: source})
	}
}

// publishQuotaBindings performs a live projection and publishes bounded immutable
// evidence before returning, so an identity GET immediately arms the dashboard.
func (s *Store) publishQuotaBindings() []QuotaBinding {
	if s == nil {
		return nil
	}
	source := s.quotaSource.Load()
	if source == nil {
		return nil
	}
	sequence := source.reads.Add(1)
	bindings := source.source.QuotaBindings()
	source.publish(sequence, bindings)
	return bindings
}

// publishedQuotaBindings is an atomic advisory read for original handlers. It
// never queries the source and therefore never waits on core credential locks.
func (s *Store) publishedQuotaBindings() []QuotaBinding {
	if s == nil {
		return nil
	}
	source := s.quotaSource.Load()
	if source == nil {
		return nil
	}
	if snapshot := source.snapshot(); snapshot != nil {
		return snapshot.bindings
	}
	return nil
}

func (s *Store) publishedQuotaBinding(provider, index string) (QuotaBinding, bool) {
	if s == nil {
		return QuotaBinding{}, false
	}
	source := s.quotaSource.Load()
	if source == nil {
		return QuotaBinding{}, false
	}
	snapshot := source.snapshot()
	if snapshot == nil {
		return QuotaBinding{}, false
	}
	return snapshot.index.binding(provider, index)
}

func (s *Store) quotaIdentitiesHTTP(c *gin.Context) {
	bindings := s.publishQuotaBindings()
	public := make([]publicQuotaBinding, 0, len(bindings))
	for _, binding := range bindings {
		public = append(public, quotaBindingPublic(binding))
	}
	c.JSON(http.StatusOK, gin.H{"bindings": public})
}

// quotaAPICallProof is intentionally fail-closed. A literal bearer, even with a
// valid index, proves nothing: only the original handler's known $TOKEN$ placement
// is accepted. When the provider has operation selectors, the request must supply
// exactly the digests this credential published; otherwise a captured response
// could be attributed to a credential the request never addressed.
func quotaAPICallProof(binding QuotaBinding, rawURL string, headers map[string]string, data string) bool {
	if len(headers) > 32 {
		return false
	}
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Fragment != "" {
		return false
	}
	for key, values := range endpoint.Query() {
		if binding.Provider != "xai" || key != "format" || len(values) != 1 || values[0] != "credits" {
			return false
		}
	}
	normalized := map[string]string{}
	for key, value := range headers {
		key = strings.ToLower(strings.TrimSpace(key))
		if _, exists := normalized[key]; exists {
			return false
		}
		switch key {
		case "authorization", "chatgpt-account-id", "accept", "content-type", "user-agent", "anthropic-beta", "anthropic-version", "origin", "referer":
		default:
			return false
		}
		normalized[key] = strings.TrimSpace(value)
	}
	if normalized["authorization"] != "Bearer $TOKEN$" {
		return false
	}
	selectors := map[string]string{}
	if account := normalized["chatgpt-account-id"]; account != "" {
		if binding.Provider != "codex" || strings.Contains(account, "$TOKEN$") {
			return false
		}
		selectors["account_id"] = quotaHash(account)
	}
	if strings.TrimSpace(data) != "" && strings.TrimSpace(data) != "{}" {
		var body map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &body) != nil || len(body) != 1 {
			return false
		}
		switch {
		case binding.Provider == "codex" && endpoint.Hostname() == "chatgpt.com" && endpoint.Path == "/backend-api/wham/rate-limit-reset-credits/consume":
			// The original quota action supplies an idempotency ID, not another
			// account selector. The confirmed reset response is checked separately.
			var requestID string
			if json.Unmarshal(body["redeem_request_id"], &requestID) != nil || strings.TrimSpace(requestID) == "" || !safeCacheText(requestID, 128) {
				return false
			}
		case binding.Provider == "antigravity":
			var project string
			if json.Unmarshal(body["project"], &project) != nil || project == "" {
				return false
			}
			selectors["project_id"] = quotaHash(strings.TrimSpace(project))
		default:
			return false
		}
	}
	if len(selectors) != len(binding.SelectorHashes) {
		return false
	}
	for key, hash := range binding.SelectorHashes {
		if selectors[key] != hash {
			return false
		}
	}
	return true
}

// accountProperty names the single credential property that identifies an account
// for a provider.
//
// There is deliberately no fallback. An account is identified by exactly one
// property, so an unrelated metadata edit cannot silently re-key a credential, and
// an identifier that a provider regenerates (a uuid minted per session) is never
// consulted. A credential that does not expose its property has no account fact
// and is grouped by provider alone.
func accountProperty(provider string) string {
	if provider == "kimi" {
		// Kimi issues no email; its credential carries the device it belongs to.
		return "device_id"
	}
	return "email"
}

// quotaInformationalFields and quotaCredentialFields bound which metadata a
// projection tolerates. They are an allowlist, not an identity input: an
// unaudited key is refused rather than ignored, because an unrecognized field
// could change which account actually serves a request. No field below takes part
// in identity — identity is the provider's single account property.
var quotaInformationalFields = strings.Fields("type auth_kind email label note disabled priority weight prefix proxy_url excluded_models oauth_model_aliases model_aliases models request_retry request_scoped_errors disable_cooling websockets tool_prefix_disabled last_refresh last_refreshed_at expired expires_at expiry expires_in expire expires token_type scope scopes plan_type plan tier subscription created_at updated_at timestamp dca_expired dca_expires_at subs_tier_name subs_tier_id is_subs_active has_payment_method organization_name org_name user_name username name picture avatar claude_device_ids fingerprint_profile device_id device_seed refresh_token refreshToken id_token idToken token_endpoint redirect_uri")
var quotaCredentialFields = strings.Fields("access_token accessToken token Token api_key session_token dca_token account_id account_uuid organization_uuid organization_id org_id project_id team_id user_id sub domain base_url")

// quotaOperationSelectors are the per-provider selectors a browser-originated
// manual quota fetch supplies and that must match the credential before the
// capture is trusted. They are not identity.
var quotaOperationSelectors = map[string][]string{
	"codex":       {"account_id"},
	"antigravity": {"project_id"},
	"xai":         {"sub", "user_id"},
}

func operationSelectorKnown(provider, name string) bool {
	for _, candidate := range quotaOperationSelectors[provider] {
		if candidate == name {
			return true
		}
	}
	return false
}

func accountValue(auth *coreauth.Auth, property string) string {
	if auth == nil || property == "" {
		return ""
	}
	if raw, exists := auth.Metadata[property]; exists && raw != nil {
		if value, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return strings.TrimSpace(auth.Attributes[property])
}

func selectorValue(auth *coreauth.Auth, name string) string {
	return accountValue(auth, name)
}

// quotaHash is a one-way digest for operation selectors, which are compared but
// never displayed or persisted.
func quotaHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validTokenHash(hash string) bool {
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

type managerQuotaIdentitySource struct {
	current func() *coreauth.Manager
}

// NewQuotaIdentitySource projects the existing manager without changing core
// fields.
func NewQuotaIdentitySource(current func() *coreauth.Manager) QuotaIdentitySource {
	return &managerQuotaIdentitySource{current: current}
}

// quotaAuthCatalog is bounded and local to a synchronous source call. Count
// addresses before projection: a duplicated index or display key must not appear
// unambiguous.
type quotaAuthCatalog struct {
	byIndex map[string]*coreauth.Auth
	keys    map[string]int
}

func quotaAuthKey(auth *coreauth.Auth) string {
	key := strings.TrimSpace(auth.FileName)
	if key == "" {
		key = auth.ID
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "devin" {
		key += "\x00" + auth.EnsureIndex()
	}
	return provider + "\x00" + key
}

func newQuotaAuthCatalog(auths []*coreauth.Auth) quotaAuthCatalog {
	if len(auths) > maxQuotaIdentities {
		return quotaAuthCatalog{}
	}
	catalog := quotaAuthCatalog{byIndex: make(map[string]*coreauth.Auth, len(auths)), keys: make(map[string]int, len(auths))}
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		index := auth.EnsureIndex()
		if _, exists := catalog.byIndex[index]; exists {
			catalog.byIndex[index] = nil
		} else {
			catalog.byIndex[index] = auth
		}
		catalog.keys[quotaAuthKey(auth)]++
	}
	return catalog
}

func (catalog quotaAuthCatalog) auth(provider, index string) *coreauth.Auth {
	auth := catalog.byIndex[strings.TrimSpace(index)]
	if auth == nil || catalog.keys[quotaAuthKey(auth)] != 1 || (provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), strings.TrimSpace(provider))) {
		return nil
	}
	return auth
}

func (source *managerQuotaIdentitySource) QuotaBindings() []QuotaBinding {
	if source == nil || source.current == nil {
		return nil
	}
	manager := source.current()
	if manager == nil {
		return nil
	}
	catalog := newQuotaAuthCatalog(manager.List())
	result := make([]QuotaBinding, 0, len(catalog.byIndex))
	for index := range catalog.byIndex {
		binding, ok := ProjectQuotaBinding(catalog.auth("", index))
		if !ok {
			continue
		}
		result = append(result, binding)
	}
	if source.current() != manager {
		return nil
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].Key < result[j].Key
	})
	return result
}

// ProjectQuotaBinding is a bounded, restart-stable projection of the facts that
// identify a credential: its provider, its account and its display key. It returns
// no token and no selector value.
func ProjectQuotaBinding(auth *coreauth.Auth) (QuotaBinding, bool) {
	if auth == nil {
		return QuotaBinding{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if _, known := quotaCacheSchemas[provider]; !known {
		return QuotaBinding{}, false
	}
	// Fail closed on anything that could change which account actually serves a
	// request: an unaudited metadata key, or a custom header that can replace
	// authorization or select an account independently of the projected facts.
	if len(auth.Metadata) > 128 || len(auth.Attributes) > 128 || len(auth.ID) > 1024 {
		return QuotaBinding{}, false
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(strings.ToLower(key), "header:") {
			return QuotaBinding{}, false
		}
	}
	known := map[string]bool{}
	for _, field := range quotaInformationalFields {
		known[field] = true
	}
	for _, field := range quotaCredentialFields {
		known[field] = true
	}
	for key, value := range auth.Metadata {
		if !known[key] {
			return QuotaBinding{}, false
		}
		if str, ok := value.(string); ok && len(str) > 16384 {
			return QuotaBinding{}, false
		}
	}
	// The credential's declared type must agree with the provider it is filed under.
	if typ, exists := auth.Metadata["type"]; exists && typ != nil && typ != "" {
		value, ok := typ.(string)
		if !ok || strings.ToLower(strings.TrimSpace(value)) != provider {
			return QuotaBinding{}, false
		}
	}
	index := auth.EnsureIndex()
	key := strings.TrimSpace(auth.FileName)
	if key == "" {
		key = auth.ID
	}
	if key == "" || index == "" {
		return QuotaBinding{}, false
	}
	if provider == "devin" {
		key += "\x00" + index
	}
	if len(key) > 512 || !safeCacheText(strings.ReplaceAll(key, "\x00", ""), 512) || !safeCacheText(index, 256) {
		return QuotaBinding{}, false
	}
	kind := accountProperty(provider)
	account := accountValue(auth, kind)
	// An account fact is an opaque label used in a storage key, so it must stay
	// short, printable and free of the key separator.
	if account != "" && (len(account) > 256 || !safeCacheText(account, 256) || strings.Contains(account, ":")) {
		return QuotaBinding{}, false
	}
	binding := QuotaBinding{Provider: provider, Key: key, Account: account, AuthIndex: index, SelectorHashes: map[string]string{}}
	if account != "" {
		binding.AccountKind = kind
	}
	for _, name := range quotaOperationSelectors[provider] {
		if value := selectorValue(auth, name); value != "" && len(value) <= 16384 {
			binding.SelectorHashes[name] = quotaHash(value)
		}
	}
	return binding, true
}
