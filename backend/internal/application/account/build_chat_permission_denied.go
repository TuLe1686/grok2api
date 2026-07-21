package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// BuildChatPermissionDeniedConfig controls runtime disable + timed Build chat probes.
// Mapped from runtime config in the app layer; this package does not import infra/config.
type BuildChatPermissionDeniedConfig struct {
	RequestDisable     bool
	InspectEnabled     bool
	InspectInterval    time.Duration
	InspectConcurrency int
}

const (
	buildChatPermissionDeniedReason = "Build chat endpoint permission-denied"
	buildChatInspectProbeModel      = "grok-3"
	buildChatInspectBatchSize       = 100
	buildChatInspectMaxScans        = 50
	buildChatInspectMaxDisables     = 100
	buildChatInspectLockKey         = "account-build-chat-permission:inspect"
	buildChatInspectLockTTL         = 10 * time.Minute
	buildChatInspectRunTimeout      = 8 * time.Minute
	buildChatInspectProbeTimeout    = 45 * time.Second
)

// UpdateBuildChatPermissionDeniedConfig hot-reloads the Build chat permission-denied policy.
func (s *Service) UpdateBuildChatPermissionDeniedConfig(value BuildChatPermissionDeniedConfig) {
	value = normalizeBuildChatPermissionDeniedConfig(value)
	s.buildChatDenyMu.Lock()
	if s.buildChatDeny == value {
		s.buildChatDenyMu.Unlock()
		return
	}
	s.buildChatDeny = value
	s.buildChatDenyRevision++
	s.buildChatDenyMu.Unlock()
	select {
	case s.buildChatDenyWake <- struct{}{}:
	default:
	}
}

func normalizeBuildChatPermissionDeniedConfig(value BuildChatPermissionDeniedConfig) BuildChatPermissionDeniedConfig {
	if value.InspectInterval < time.Minute {
		value.InspectInterval = time.Minute
	}
	if value.InspectInterval > 24*time.Hour {
		value.InspectInterval = 24 * time.Hour
	}
	if value.InspectConcurrency < 1 {
		value.InspectConcurrency = 1
	}
	if value.InspectConcurrency > 32 {
		value.InspectConcurrency = 32
	}
	return value
}

func (s *Service) buildChatPermissionDeniedSnapshot() (BuildChatPermissionDeniedConfig, uint64) {
	s.buildChatDenyMu.RLock()
	defer s.buildChatDenyMu.RUnlock()
	return s.buildChatDeny, s.buildChatDenyRevision
}

func (s *Service) buildChatPermissionDeniedConfig() BuildChatPermissionDeniedConfig {
	value, _ := s.buildChatPermissionDeniedSnapshot()
	return value
}

func (s *Service) RequestDisableBuildChatPermissionDenied() bool {
	return s.buildChatPermissionDeniedConfig().RequestDisable
}

func (s *Service) buildChatPermissionDeniedRevisionCurrent(expected uint64, cfg BuildChatPermissionDeniedConfig) bool {
	current, revision := s.buildChatPermissionDeniedSnapshot()
	return revision == expected && current == cfg
}

func buildChatInspectInterval(cfg BuildChatPermissionDeniedConfig) time.Duration {
	if !cfg.InspectEnabled {
		return time.Hour
	}
	return normalizeBuildChatPermissionDeniedConfig(cfg).InspectInterval
}

// IsBuildChatPermissionDenied reports the upstream chat permission-denied signature.
func IsBuildChatPermissionDenied(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	code, _, message := extractUpstreamErrorMetadata(body)
	metadata := strings.ToLower(strings.Join([]string{code, message, string(body)}, " "))
	if strings.Contains(metadata, "access to the chat endpoint is denied") {
		return true
	}
	return normalizeFailureCode(code) == "permission_denied" && strings.Contains(metadata, "chat endpoint")
}

// DisableBuildChatPermissionDenied disables a Build account after chat permission-denied.
// Non-Build providers are ignored so Web/Console behavior stays unchanged.
func (s *Service) DisableBuildChatPermissionDenied(ctx context.Context, credential accountdomain.Credential, reason string) error {
	if credential.Provider != accountdomain.ProviderBuild || credential.ID == 0 {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = buildChatPermissionDeniedReason
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.DisableAccount(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("build_chat_permission_denied_disable_failed", "account_id", credential.ID, "error", err)
		return err
	}
	s.logger.Warn("build_chat_permission_denied_disabled", "account_id", credential.ID, "name", credential.Name, "reason", reason)
	return nil
}

// RunBuildChatPermissionDeniedInspect periodically probes enabled Build accounts and disables
// those that return chat endpoint permission-denied. Default policy enables this path.
func (s *Service) RunBuildChatPermissionDeniedInspect(ctx context.Context) {
	select {
	case <-s.buildChatDenyWake:
	default:
	}
	cfg, scheduledRevision := s.buildChatPermissionDeniedSnapshot()
	timer := time.NewTimer(buildChatInspectInterval(cfg))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.buildChatDenyWake:
			cfg, scheduledRevision = s.buildChatPermissionDeniedSnapshot()
			resetCredentialRefreshTimer(timer, buildChatInspectInterval(cfg))
		case <-timer.C:
			current, revision := s.buildChatPermissionDeniedSnapshot()
			if current.InspectEnabled && revision == scheduledRevision {
				if err := s.runBuildChatPermissionDeniedInspectRevision(ctx, current, revision); err != nil && ctx.Err() == nil {
					s.logger.Warn("build_chat_permission_denied_inspect_failed", "error", err)
				}
			}
			cfg, scheduledRevision = s.buildChatPermissionDeniedSnapshot()
			resetCredentialRefreshTimer(timer, buildChatInspectInterval(cfg))
		}
	}
}

func (s *Service) runBuildChatPermissionDeniedInspect(ctx context.Context, cfg BuildChatPermissionDeniedConfig) error {
	_, revision := s.buildChatPermissionDeniedSnapshot()
	return s.runBuildChatPermissionDeniedInspectRevision(ctx, cfg, revision)
}

func (s *Service) runBuildChatPermissionDeniedInspectRevision(ctx context.Context, cfg BuildChatPermissionDeniedConfig, revision uint64) error {
	cfg = normalizeBuildChatPermissionDeniedConfig(cfg)
	if !cfg.InspectEnabled || !s.buildChatPermissionDeniedRevisionCurrent(revision, cfg) {
		return nil
	}
	runCtx, cancel := context.WithTimeout(ctx, buildChatInspectRunTimeout)
	defer cancel()
	var release func()
	if s.refreshLock != nil {
		lockRelease, acquired, err := s.refreshLock.Acquire(runCtx, buildChatInspectLockKey, buildChatInspectLockTTL)
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		release = lockRelease
		defer release()
	}

	adapter, ok := s.providers.Responses(accountdomain.ProviderBuild)
	if !ok {
		return fmt.Errorf("build response adapter unavailable")
	}

	var (
		scanned, scanBatches int
		disabled             atomic.Int64
		probeErrors          atomic.Int64
		afterID              uint64
	)
	for scanBatches < buildChatInspectMaxScans && int(disabled.Load()) < buildChatInspectMaxDisables {
		if !s.buildChatPermissionDeniedRevisionCurrent(revision, cfg) {
			return nil
		}
		values, _, err := s.accounts.ListProviderAccountBatch(runCtx, accountdomain.ProviderBuild, afterID, buildChatInspectBatchSize)
		if err != nil {
			return err
		}
		if len(values) == 0 {
			break
		}
		scanBatches++
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			afterID = value.ID
			if !value.Enabled || value.AuthStatus != accountdomain.AuthStatusActive {
				continue
			}
			ids = append(ids, value.ID)
		}
		scanned += len(ids)
		if len(ids) == 0 {
			if len(values) < buildChatInspectBatchSize {
				break
			}
			continue
		}
		pool := s.refreshPool
		if pool == nil {
			return fmt.Errorf("build chat inspect pool unavailable")
		}
		_, batchFailed, err := s.runAccountBatch(runCtx, "build_chat_permission_denied_inspect", ids, pool, nil, func(workCtx context.Context, id uint64) error {
			if int(disabled.Load()) >= buildChatInspectMaxDisables {
				return nil
			}
			if !s.buildChatPermissionDeniedRevisionCurrent(revision, cfg) {
				return nil
			}
			taskCtx, taskCancel := context.WithTimeout(workCtx, buildChatInspectProbeTimeout)
			defer taskCancel()
			credential, getErr := s.accounts.Get(taskCtx, id)
			if getErr != nil {
				return getErr
			}
			if !credential.Enabled || credential.AuthStatus != accountdomain.AuthStatusActive || credential.Provider != accountdomain.ProviderBuild {
				return nil
			}
			status, body, probeErr := s.probeBuildChatPermission(taskCtx, adapter, credential)
			if probeErr != nil {
				probeErrors.Add(1)
				return nil
			}
			if !IsBuildChatPermissionDenied(status, body) {
				return nil
			}
			if err := s.DisableBuildChatPermissionDenied(taskCtx, credential, buildChatPermissionDeniedReason); err != nil {
				return err
			}
			disabled.Add(1)
			return nil
		})
		if err != nil {
			return err
		}
		probeErrors.Add(int64(batchFailed))
		if len(values) < buildChatInspectBatchSize {
			break
		}
	}
	if disabled.Load() > 0 || scanned > 0 {
		s.logger.Info("build_chat_permission_denied_inspect",
			"scanned", scanned, "disabled", disabled.Load(), "probe_errors", probeErrors.Load(), "scan_batches", scanBatches,
			"interval", cfg.InspectInterval.String(), "concurrency", cfg.InspectConcurrency,
		)
	}
	return nil
}

func (s *Service) probeBuildChatPermission(ctx context.Context, adapter provider.ResponseAdapter, credential accountdomain.Credential) (int, []byte, error) {
	usable, err := s.EnsureCredential(ctx, credential, false)
	if err != nil {
		return 0, nil, err
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping","stream":false,"max_output_tokens":1}`, buildChatInspectProbeModel))
	response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{
		Credential:    usable,
		Method:        http.MethodPost,
		Path:          "/responses",
		Model:         buildChatInspectProbeModel,
		Body:          body,
		Streaming:     false,
		NormalizeBody: true,
		Operation:     "responses",
	})
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		return response.StatusCode, nil, readErr
	}
	return response.StatusCode, payload, nil
}

func extractUpstreamErrorMetadata(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", strings.TrimSpace(string(body))
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code := firstNonEmptyString(firstMapString(nested, "code", "error_code"), firstMapString(root, "code", "error_code"))
		errorType := firstNonEmptyString(firstMapString(nested, "type", "error_type"), firstMapString(root, "type", "error_type"))
		message := firstNonEmptyString(firstMapString(nested, "message", "error"), firstMapString(root, "message"))
		return code, errorType, message
	}
	message := firstNonEmptyString(firstMapString(root, "error"), firstMapString(root, "message"))
	return firstMapString(root, "code", "error_code"), firstMapString(root, "type", "error_type"), message
}

func firstMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeFailureCode(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
