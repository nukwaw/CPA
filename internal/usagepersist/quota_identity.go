package usagepersist

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// QuotaBinding exposes only opaque identity/fence values. Proof fields are
// transient, bounded hashes; neither credentials nor account metadata escape.
type QuotaBinding struct {
	Provider             string            `json:"provider"`
	Key                  string            `json:"key"`
	AuthIndex            string            `json:"auth_index"`
	CredentialGeneration string            `json:"credential_generation"`
	Revision             string            `json:"revision"`
	AccessTokenSHA256    string            `json:"-"`
	APITokenSHA256       string            `json:"-"`
	TokenScoped          bool              `json:"-"`
	SelectorHashes       map[string]string `json:"-"`
	Lifetime             string            `json:"-"`
	RuntimeGeneration    uint64            `json:"-"`
}

// QuotaIdentitySource must return immutable, sanitized projections. Both methods
// may block on credential-manager locks, even when validateStorage is false:
// core can hold those locks during storage I/O. Only the independent worker and
// explicit add-on APIs may call a source. Original handlers use a published copy.
type QuotaIdentitySource interface {
	QuotaBindings(validateStorage bool) []QuotaBinding
}

// TargetedQuotaIdentitySource optionally avoids validating unrelated backing
// files during a locked mutation. It must resolve current credentials and refuse
// ambiguous indexes/keys, not reuse a previously validated binding. It has the same
// possibly-blocking source contract as QuotaBindings, regardless of the bool.
type TargetedQuotaIdentitySource interface {
	QuotaBinding(provider, index string, validateStorage bool) (QuotaBinding, bool)
}

type quotaSourceHolder struct {
	source    QuotaIdentitySource
	reads     atomic.Uint64
	stopped   atomic.Bool
	published atomic.Pointer[quotaIdentityPublication]
}

// Published bindings are advisory request-start evidence, never write authority.
// They contain only bounded projection copies and can be stale indefinitely; the
// worker revalidates captured generation/revision against the live source before
// writing. No available publication means original quota observations are skipped.
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
		binding.AuthIndex = strings.Clone(binding.AuthIndex)
		binding.CredentialGeneration = strings.Clone(binding.CredentialGeneration)
		binding.Revision = strings.Clone(binding.Revision)
		binding.AccessTokenSHA256 = strings.Clone(binding.AccessTokenSHA256)
		binding.APITokenSHA256 = strings.Clone(binding.APITokenSHA256)
		binding.Lifetime = strings.Clone(binding.Lifetime)
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
	if _, known := quotaCacheSchemas[binding.Provider]; !known || binding.Key == "" || !safeCacheText(strings.ReplaceAll(binding.Key, "\x00", ""), 512) || len(binding.Key) > 512 || binding.AuthIndex == "" || !safeCacheText(binding.AuthIndex, 256) || len(binding.SelectorHashes) > len(quotaSelectorFields) {
		return false
	}
	if !strings.HasPrefix(binding.CredentialGeneration, "qg1:") || !validTokenHash(strings.TrimPrefix(binding.CredentialGeneration, "qg1:")) || !validTokenHash(binding.Revision) {
		return false
	}
	for _, hash := range []string{binding.AccessTokenSHA256, binding.APITokenSHA256, binding.Lifetime} {
		if hash != "" && !validTokenHash(hash) {
			return false
		}
	}
	for key, hash := range binding.SelectorHashes {
		known := false
		for _, selector := range quotaSelectorFields {
			known = known || key == selector
		}
		if !known || !validTokenHash(hash) {
			return false
		}
	}
	return true
}

const maxQuotaIdentities = 4096

// quotaIdentityIndex contains only sanitized projections, never credentials.
// Live read/write validation and advisory immutable publications use separate
// instances; cached evidence is never used as mutation authority.
type quotaIdentityIndex map[string]QuotaBinding

func indexQuotaBindings(bindings []QuotaBinding) quotaIdentityIndex {
	if len(bindings) > maxQuotaIdentities {
		return nil
	}
	byIndex := make(quotaIdentityIndex, len(bindings))
	keys := make(map[string]int, len(bindings))
	for _, binding := range bindings {
		keys[binding.Provider+"\x00"+binding.Key]++
		if _, exists := byIndex[binding.AuthIndex]; exists {
			byIndex[binding.AuthIndex] = QuotaBinding{} // Duplicate indexes stay refused.
		} else {
			byIndex[binding.AuthIndex] = binding
		}
	}
	for index, binding := range byIndex {
		if keys[binding.Provider+"\x00"+binding.Key] != 1 {
			byIndex[index] = QuotaBinding{}
		}
	}
	return byIndex
}

func (bindings quotaIdentityIndex) binding(provider, index string) (QuotaBinding, bool) {
	binding := bindings[strings.TrimSpace(index)]
	return binding, binding.AuthIndex != "" && (provider == "" || binding.Provider == strings.ToLower(strings.TrimSpace(provider)))
}

func (bindings quotaIdentityIndex) valid(provider, index, generation, revision string) bool {
	binding, ok := bindings.binding(provider, index)
	return ok && generation != "" && binding.CredentialGeneration == generation && (revision == "" || binding.Revision == revision)
}

func (s *Store) quotaIdentitySnapshot(validateStorage bool) quotaIdentityIndex {
	if !validateStorage {
		if s != nil {
			if snapshot := s.quotaSource.Load().snapshot(); snapshot != nil {
				return snapshot.index
			}
		}
		return nil
	}
	return indexQuotaBindings(s.quotaBindings(true))
}

var ErrQuotaIdentity = errors.New("quota credential binding is stale or unknown")

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

// false is an atomic advisory read, not a live "memory-only" manager lookup.
// true is authoritative and may block: only explicit add-on APIs and independent
// worker validation use it. A successful full lookup publishes bounded immutable
// evidence before returning, so an identity GET can immediately arm the UI.
func (s *Store) quotaBindings(validateStorage bool) []QuotaBinding {
	if s == nil {
		return nil
	}
	source := s.quotaSource.Load()
	if source == nil {
		return nil
	}
	if !validateStorage {
		if snapshot := source.snapshot(); snapshot != nil {
			return snapshot.bindings
		}
		return nil
	}
	sequence := source.reads.Add(1)
	bindings := source.source.QuotaBindings(true)
	source.publish(sequence, bindings)
	return bindings
}
func (s *Store) quotaBinding(provider, index string, validateStorage bool) (QuotaBinding, bool) {
	if s == nil {
		return QuotaBinding{}, false
	}
	if !validateStorage {
		return s.quotaIdentitySnapshot(false).binding(provider, index)
	}
	if source := s.quotaSource.Load(); source != nil {
		if targeted, ok := source.source.(TargetedQuotaIdentitySource); ok {
			return targeted.QuotaBinding(provider, index, true)
		}
	}
	return s.quotaIdentitySnapshot(true).binding(provider, index)
}
func (s *Store) validQuotaIdentity(provider, index, generation, revision string, validateStorage bool) bool {
	if generation == "" {
		return false
	}
	b, ok := s.quotaBinding(provider, index, validateStorage)
	return ok && b.CredentialGeneration == generation && (revision == "" || b.Revision == revision)
}
func (s *Store) quotaIdentitiesHTTP(c *gin.Context) {
	bindings := s.quotaBindings(true)
	public := make([]publicQuotaBinding, 0, len(bindings))
	for _, binding := range bindings {
		public = append(public, quotaBindingPublic(binding))
	}
	c.JSON(http.StatusOK, gin.H{"bindings": public})
}

// quotaAPICallProof is intentionally fail-closed. auth_index only selects a
// proxy/credential route; a literal bearer (even with a valid index) proves
// nothing. Only the existing handler's known $TOKEN$ placement is accepted.
func quotaAPICallProof(binding QuotaBinding, rawURL string, headers map[string]string, data string) bool {
	if binding.APITokenSHA256 == "" || len(headers) > 32 {
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

func quotaHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func quotaDigest(parts []string) string {
	data, _ := json.Marshal(parts)
	return "qg1:" + quotaHash(string(data))
}

// TokenQuotaGeneration derives identity solely from producer-supplied evidence.
// It is eligible only for verified token-scoped projections (never Codex).
func TokenQuotaGeneration(provider, hash string) string {
	return quotaDigest([]string{"quota-credential-v1", provider, "access_token", hash})
}
func validTokenHash(hash string) bool {
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

type managerQuotaIdentitySource struct {
	current  func() *coreauth.Manager
	validate func(*coreauth.Auth) bool
	nonce    string
}

// NewQuotaIdentitySource projects the existing manager without changing core
// fields. validate, if supplied, runs only on add-on read/write/worker paths.
func NewQuotaIdentitySource(current func() *coreauth.Manager, validate func(*coreauth.Auth) bool) QuotaIdentitySource {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil
	}
	return &managerQuotaIdentitySource{current: current, validate: validate, nonce: hex.EncodeToString(nonce)}
}

// quotaAuthCatalog is bounded and local to a synchronous source call. Count
// addresses before projection/disk validation: an invalid competing credential
// must not make a duplicated index or display key appear unambiguous.
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

func sameQuotaAuthRevision(before, after *coreauth.Auth) bool {
	return before != nil && after != nil && before.ID == after.ID && before.Index == after.Index && quotaAuthKey(before) == quotaAuthKey(after) && before.RegistrationEpoch == after.RegistrationEpoch && before.Generation == after.Generation
}

func (source *managerQuotaIdentitySource) project(auth *coreauth.Auth) (QuotaBinding, bool) {
	binding, ok := ProjectQuotaBinding(auth)
	if !ok {
		return QuotaBinding{}, false
	}
	lifetime, _ := json.Marshal([]any{source.nonce, auth.ID, auth.RegistrationEpoch})
	revision, _ := json.Marshal([]any{source.nonce, auth.ID, auth.RegistrationEpoch, auth.Generation})
	binding.Lifetime = quotaHash(string(lifetime))
	binding.Revision = quotaHash(string(revision))
	binding.RuntimeGeneration = auth.Generation
	return binding, true
}

func (source *managerQuotaIdentitySource) QuotaBindings(validateStorage bool) []QuotaBinding {
	if source.current == nil {
		return nil
	}
	manager := source.current()
	if manager == nil {
		return nil
	}
	catalog := newQuotaAuthCatalog(manager.List())
	result := make([]QuotaBinding, 0, len(catalog.byIndex))
	for index := range catalog.byIndex {
		auth := catalog.auth("", index)
		binding, ok := source.project(auth)
		if !ok || (validateStorage && source.validate != nil && !source.validate(auth)) {
			continue
		}
		result = append(result, binding)
	}
	if validateStorage {
		// I/O may have overlapped replacement, removal, or new ambiguity. Recheck
		// the entire catalog once, in memory, rather than trusting the old clones.
		current := newQuotaAuthCatalog(manager.List())
		filtered := result[:0]
		for _, binding := range result {
			if sameQuotaAuthRevision(catalog.auth(binding.Provider, binding.AuthIndex), current.auth(binding.Provider, binding.AuthIndex)) {
				filtered = append(filtered, binding)
			}
		}
		result = filtered
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

func (source *managerQuotaIdentitySource) QuotaBinding(provider, index string, validateStorage bool) (QuotaBinding, bool) {
	if source.current == nil {
		return QuotaBinding{}, false
	}
	manager := source.current()
	if manager == nil {
		return QuotaBinding{}, false
	}
	// Core has no public catalog version/index lookup. A fresh memory-only scan
	// is necessary to refuse newly introduced ambiguity; never cache bindings.
	catalog := newQuotaAuthCatalog(manager.List())
	auth := catalog.auth(provider, index)
	if auth == nil {
		return QuotaBinding{}, false
	}
	current, exists := manager.GetByID(auth.ID)
	if !exists || !sameQuotaAuthRevision(auth, current) {
		return QuotaBinding{}, false
	}
	binding, ok := source.project(current)
	if !ok || (validateStorage && source.validate != nil && !source.validate(current)) {
		return QuotaBinding{}, false
	}
	if validateStorage {
		// Only this credential's backing file was opened. Recheck its current
		// registration/revision AND competing addresses after the I/O, including
		// replacement while a queued mutation waited for its storage row lock.
		latest := newQuotaAuthCatalog(manager.List()).auth(provider, index)
		if !sameQuotaAuthRevision(current, latest) {
			return QuotaBinding{}, false
		}
	}
	return binding, source.current() == manager
}

// Known non-identity fields may be ignored. Unknown metadata is deliberately not
// guessed: a plugin may give it credential/selector semantics we cannot prove.
// Core metadata_keys.go, auth overrides/model aliases, and the original
// auth_files_fields.go support these canonical retry/routing/transport settings.
// They do not select quota credentials/accounts. Custom headers and unknown
// plugin settings are deliberately NOT included.
var quotaInformationalFields = strings.Fields("type auth_kind email label note disabled priority weight prefix proxy_url excluded_models oauth_model_aliases model_aliases models request_retry request_scoped_errors disable_cooling websockets tool_prefix_disabled last_refresh last_refreshed_at expired expires_at expiry expires_in expire expires token_type scope scopes plan_type plan tier subscription created_at updated_at timestamp dca_expired dca_expires_at subs_tier_name subs_tier_id is_subs_active has_payment_method organization_name org_name user_name username name picture avatar claude_device_ids fingerprint_profile device_id device_seed refresh_token refreshToken id_token idToken token_endpoint redirect_uri")
var quotaSelectorFields = strings.Fields("account_id account_uuid organization_uuid organization_id org_id project_id team_id user_id sub domain base_url")

// ProjectDiskQuotaBinding projects the credential a backing file currently
// describes, for comparison with the live runtime projection.
//
// Only metadata is replaced with the file's: metadata is what a file credential
// restates, while the runtime attributes are shared read-only because a file cannot
// restate them. Core derives attributes for file credentials that do take part in
// identity, such as the Kimi domain/base_url pair that every Kimi file credential
// receives, so dropping them would make every comparison fail rather than detect a
// real change to the file.
func ProjectDiskQuotaBinding(runtime *coreauth.Auth, metadata map[string]any) (QuotaBinding, bool) {
	if runtime == nil || len(metadata) > 128 {
		return QuotaBinding{}, false
	}
	return ProjectQuotaBinding(&coreauth.Auth{
		ID:         runtime.ID,
		Index:      runtime.Index,
		Provider:   runtime.Provider,
		FileName:   runtime.FileName,
		Attributes: runtime.Attributes,
		Metadata:   metadata,
	})
}

// ProjectQuotaBinding is a bounded, restart-stable projection of effective
// credentials and quota-affecting selectors. It does not use email, mtime or ID
// as account evidence. It returns no raw token or selector value.
func ProjectQuotaBinding(auth *coreauth.Auth) (QuotaBinding, bool) {
	if auth == nil || len(auth.Metadata) > 128 || len(auth.Attributes) > 128 || len(auth.ID) > 1024 {
		return QuotaBinding{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if _, known := quotaCacheSchemas[provider]; !known {
		return QuotaBinding{}, false
	}
	b := QuotaBinding{Provider: provider, AuthIndex: auth.EnsureIndex(), Key: strings.TrimSpace(auth.FileName), SelectorHashes: map[string]string{}}
	if b.Key == "" {
		b.Key = auth.ID
	}
	if b.Key == "" || !safeCacheText(b.Key, 512) || b.AuthIndex == "" || !safeCacheText(b.AuthIndex, 256) {
		return QuotaBinding{}, false
	}
	if provider == "devin" {
		b.Key += "\x00" + b.AuthIndex
	}
	if len(b.Key) > 512 {
		return QuotaBinding{}, false
	}
	known := map[string]bool{}
	for _, key := range quotaInformationalFields {
		known[key] = true
	}
	for _, key := range quotaSelectorFields {
		known[key] = true
	}
	for _, key := range []string{"access_token", "accessToken", "token", "Token", "api_key", "session_token", "dca_token"} {
		known[key] = true
	}
	for key, value := range auth.Metadata {
		if !known[key] {
			return QuotaBinding{}, false
		}
		if str, ok := value.(string); ok && len(str) > 16384 {
			return QuotaBinding{}, false
		}
	}
	// Custom headers can independently select an account or replace authorization.
	for key := range auth.Attributes {
		if strings.HasPrefix(strings.ToLower(key), "header:") {
			return QuotaBinding{}, false
		}
	}
	if typ, exists := auth.Metadata["type"]; exists && typ != nil && typ != "" {
		value, ok := typ.(string)
		if !ok || strings.ToLower(strings.TrimSpace(value)) != provider {
			return QuotaBinding{}, false
		}
	}
	scalar := func(key string) (string, bool) {
		value := ""
		if raw, exists := auth.Metadata[key]; exists && raw != nil {
			var ok bool
			value, ok = raw.(string)
			if !ok {
				return "", false
			}
			value = strings.TrimSpace(value)
		}
		attr := strings.TrimSpace(auth.Attributes[key])
		if len(value) > 16384 || len(attr) > 16384 {
			return "", false
		}
		if attr != "" {
			if value != "" && value != attr {
				return "", false
			}
			value = attr
		}
		return value, true
	}
	// OAuth spellings must agree. In particular core fingerprint and management
	// token resolution have different precedence: disagreement is not a binding.
	access := ""
	addAccess := func(v any) bool {
		if v == nil {
			return true
		}
		str, ok := v.(string)
		if !ok || len(str) > 16384 {
			return false
		}
		str = strings.TrimSpace(str)
		if str == "" {
			return true
		}
		if access != "" && access != str {
			return false
		}
		access = str
		return true
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if !addAccess(auth.Metadata[key]) || !addAccess(auth.Attributes[key]) {
			return QuotaBinding{}, false
		}
	}
	stringToken := ""
	for _, key := range []string{"token", "Token"} {
		raw := auth.Metadata[key]
		if raw == nil {
			continue
		}
		switch value := raw.(type) {
		case string:
			if len(value) > 16384 {
				return QuotaBinding{}, false
			}
			value = strings.TrimSpace(value)
			if stringToken != "" && stringToken != value {
				return QuotaBinding{}, false
			}
			stringToken = value
		case map[string]any:
			if len(value) > 16 {
				return QuotaBinding{}, false
			}
			for k := range value {
				if k != "access_token" && k != "accessToken" && k != "refresh_token" && k != "token_type" && k != "expiry" && k != "expires_in" && k != "expires_at" && k != "scope" {
					return QuotaBinding{}, false
				}
			}
			if !addAccess(value["access_token"]) || !addAccess(value["accessToken"]) {
				return QuotaBinding{}, false
			}
		case map[string]string:
			if len(value) > 16 {
				return QuotaBinding{}, false
			}
			for k := range value {
				if k != "access_token" && k != "accessToken" && k != "refresh_token" && k != "token_type" && k != "expiry" && k != "expires_in" && k != "expires_at" && k != "scope" {
					return QuotaBinding{}, false
				}
			}
			if !addAccess(value["access_token"]) || !addAccess(value["accessToken"]) {
				return QuotaBinding{}, false
			}
		default:
			return QuotaBinding{}, false
		}
	}
	apiKey, ok := scalar("api_key")
	if !ok {
		return QuotaBinding{}, false
	}
	session, ok := scalar("session_token")
	if !ok {
		return QuotaBinding{}, false
	}
	dca, ok := scalar("dca_token")
	if !ok {
		return QuotaBinding{}, false
	}
	if attr := strings.TrimSpace(auth.Attributes["token"]); attr != "" {
		if stringToken != "" && stringToken != attr {
			return QuotaBinding{}, false
		}
		stringToken = attr
	}
	effective := ""
	// Primary credential aliases must not disagree; DCA is an independent Meta
	// acquisition credential and is hashed separately from the minted API key.
	for _, value := range []string{access, apiKey, session, stringToken} {
		if value == "" {
			continue
		}
		if effective != "" && effective != value {
			return QuotaBinding{}, false
		}
		effective = value
	}
	if dca != "" && provider != "meta" {
		return QuotaBinding{}, false
	}
	if effective == "" {
		if provider != "meta" || dca == "" {
			return QuotaBinding{}, false
		}
		effective = dca
	}
	parts := []string{"quota-credential-v1", provider, "access_token", quotaHash(effective)}
	if dca != "" && dca != effective {
		parts = append(parts, "dca_token", quotaHash(dca))
	}
	for _, key := range quotaSelectorFields {
		value, valid := scalar(key)
		if !valid {
			return QuotaBinding{}, false
		}
		if value != "" {
			hash := quotaHash(value)
			b.SelectorHashes[key] = hash
			parts = append(parts, key, hash)
		}
	}
	b.CredentialGeneration = quotaDigest(parts)
	b.AccessTokenSHA256 = coreauth.AccessTokenSHA256(auth)
	if b.AccessTokenSHA256 != "" && b.AccessTokenSHA256 != quotaHash(effective) {
		return QuotaBinding{}, false
	}
	b.TokenScoped = provider == "claude" && access != "" && b.AccessTokenSHA256 != "" && len(b.SelectorHashes) == 0 && dca == ""
	// $TOKEN$ support is intentionally narrower than manual browser cache support.
	// The capitalized Token variant is fingerprinted by core, but not API-call.
	if access != "" && (auth.Metadata["access_token"] != nil || auth.Metadata["accessToken"] != nil || auth.Metadata["token"] != nil) || apiKey != "" || session != "" || (stringToken != "" && auth.Metadata["token"] != nil) {
		b.APITokenSHA256 = quotaHash(effective)
	}
	return b, true
}
