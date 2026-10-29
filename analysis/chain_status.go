// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C1 of the chameleon-integration design: the protolsp/chainStatus
// custom request. Pull-only; the editor calls at connect / when its
// status-bar element renders. C2 added the per-file layerInfo
// request. A push notification on chain.pxf reload was sketched in
// the design doc but never implemented — `effectiveConfigChanged`
// (CP3) covers the "config changed, re-fetch" surface for status-
// bar consumers today.
//
// The result is intentionally narrow:
//   - configured: did .protolsp.pxf declare a chameleon block?
//   - chainSpecPath: where did we load it from? (empty when the
//     chameleon block exists but didn't load — see Errors)
//   - environments: declared envs + their root-first ancestry
//   - schemaFQN: the workspace's assumed @type for untyped layers
//   - errors: parse / expansion issues from chain.Load
//   - version / defaultEnvironment: surface the spec metadata so
//     the editor can show "chain spec v1.0.0, default=dev"
//
// Never errors — like the other "view" requests (registryStatus,
// getEffectiveConfig, datasetRegions), editor status-bar code
// shouldn't have to model RPC failure for what's essentially a UI
// hint.

package analysis

import (
	"fmt"
)

// MethodChainStatus is the custom request returning the workspace's
// current chameleon chain status.
const MethodChainStatus = "protolsp/chainStatus"

// ChainStatusParams is the (empty) request shape. Chain status is
// workspace-scoped — no per-document context to pass. Kept as an
// explicit type so future fields (e.g. environment selector) can
// land without breaking clients.
type ChainStatusParams struct{}

// ChainStatusEnvironment is one environment entry in the result.
type ChainStatusEnvironment struct {
	Name      string   `json:"name"`
	Ancestors []string `json:"ancestors"`
	// KeyName mirrors chain.Environment.KeyName — the name of
	// the entry in a sops PXF AgeKeyFile whose recipient is used
	// when encrypting this environment's layers. Empty when the
	// chain spec doesn't set it (the common case; the encrypt
	// path falls back to the key file's default recipient).
	KeyName string `json:"keyName,omitempty"`
}

// ChainStatusError surfaces a load- or expansion-time diagnostic
// from the chain spec. Line/Column are zero when the issue isn't
// tied to a specific token (e.g. an ancestry cycle).
type ChainStatusError struct {
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

// ChainStatusResult mirrors the design doc's
// chameleon-integration.md §"Request: protolsp/chainStatus" shape,
// extended with Version + DefaultEnvironment so the editor can
// surface "chain spec vX, default=Y" without a second call.
type ChainStatusResult struct {
	Configured         bool                     `json:"configured"`
	ChainSpecPath      string                   `json:"chainSpecPath,omitempty"`
	Environments       []ChainStatusEnvironment `json:"environments"`
	SchemaFQN          string                   `json:"schemaFQN,omitempty"`
	Errors             []ChainStatusError       `json:"errors"`
	Version            string                   `json:"version,omitempty"`
	DefaultEnvironment string                   `json:"defaultEnvironment,omitempty"`
	// ActiveEnvironment is the session-scoped env the user picked via
	// SetActiveEnv. Falls back to DefaultEnvironment when no override
	// is set — the response always carries a concrete env name when
	// the chain is configured, so editors don't have to model the
	// "unset" case themselves.
	ActiveEnvironment string `json:"activeEnvironment,omitempty"`
}

// ChainStatusSnapshot computes the current ChainStatusResult from
// the server's fileConfig + chain state. Pure function on the
// receiver — exposed at this seam so tests can assert against the
// composed shape without driving the JSON-RPC layer.
func (s *Workspace) ChainStatusSnapshot() ChainStatusResult {
	s.ws.stateMu.RLock()
	defer s.ws.stateMu.RUnlock()
	configured := s.ws.rootFileConfig != nil &&
		s.ws.rootFileConfig.Chameleon.Enabled &&
		s.ws.rootFileConfig.Chameleon.ChainSpec != ""

	result := ChainStatusResult{
		Configured:   configured,
		Environments: []ChainStatusEnvironment{},
		Errors:       []ChainStatusError{},
	}
	if !configured {
		return result
	}
	if s.ws.rootFileConfig != nil {
		result.SchemaFQN = s.ws.rootFileConfig.Chameleon.SchemaFQN
	}
	if s.ws.rootChain == nil {
		// Chain block declared but Load failed catastrophically
		// (read error, unparseable PXF). The .protolsp.pxf path is
		// the most useful hint we can give the editor here — but
		// we don't have the resolved chain path, so leave it
		// empty.
		return result
	}
	result.ChainSpecPath = s.ws.rootChain.Path
	if s.ws.rootChain.Spec != nil {
		result.Version = s.ws.rootChain.Spec.Version
		result.DefaultEnvironment = s.ws.rootChain.Spec.DefaultEnvironment
		// ActiveEnvironment reports the user-selected env when set;
		// falls back to DefaultEnvironment so the response always
		// carries a concrete answer.
		s.activeEnvMu.RLock()
		active := s.activeEnv
		s.activeEnvMu.RUnlock()
		if active == "" {
			active = s.ws.rootChain.Spec.DefaultEnvironment
		}
		result.ActiveEnvironment = active
		// Preserve declaration order (spec.Environments) rather
		// than iterating the map. Editor status display benefits
		// from the user's intended ordering.
		for _, e := range s.ws.rootChain.Spec.Environments {
			info, ok := s.ws.rootChain.Environments[e.Name]
			ancestors := []string{}
			if ok && info.Ancestors != nil {
				ancestors = info.Ancestors
			}
			result.Environments = append(result.Environments, ChainStatusEnvironment{
				Name:      e.Name,
				Ancestors: ancestors,
				KeyName:   e.KeyName,
			})
		}
	}
	for _, d := range s.ws.rootChain.Errors {
		result.Errors = append(result.Errors, ChainStatusError{
			Message: d.Message,
			Line:    d.Line,
			Column:  d.Column,
		})
	}
	return result
}

// SetActiveEnvironment validates env against the loaded chain spec
// and, on success, replaces the server's session-scoped activeEnv
// override. Returns the previous override (the value
// ChainStatusSnapshot.ActiveEnvironment would have reported just
// before the swap) so callers can confirm transitions without an
// extra round-trip.
//
// Empty env resets to "fall back to default_environment" — the
// natural "go back" gesture. Any non-empty env that isn't declared
// in the chain spec returns an error and leaves state untouched;
// the chain spec is the source of truth for which envs are valid.
//
// When the chain isn't configured we still accept empty env (idempotent
// reset) but reject non-empty values — there's no spec to validate
// against, so we'd be granting the client a fictional selection.
func (s *Workspace) SetActiveEnvironment(env string) (string, error) {
	if env != "" {
		s.ws.stateMu.RLock()
		spec := (*chainSpecSnapshot)(nil)
		if s.ws.rootChain != nil && s.ws.rootChain.Spec != nil {
			spec = &chainSpecSnapshot{
				configured: s.ws.rootFileConfig != nil &&
					s.ws.rootFileConfig.Chameleon.Enabled &&
					s.ws.rootFileConfig.Chameleon.ChainSpec != "",
				envs: make([]string, 0, len(s.ws.rootChain.Spec.Environments)),
			}
			for _, e := range s.ws.rootChain.Spec.Environments {
				spec.envs = append(spec.envs, e.Name)
			}
		}
		s.ws.stateMu.RUnlock()
		if spec == nil || !spec.configured {
			return "", fmt.Errorf("no chameleon chain configured for this workspace")
		}
		found := false
		for _, name := range spec.envs {
			if name == env {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("environment %q is not declared in the chain spec", env)
		}
	}
	s.activeEnvMu.Lock()
	prev := s.activeEnv
	s.activeEnv = env
	s.activeEnvMu.Unlock()
	// Resolve the "previous" reply through the same default-env
	// fallback ChainStatusSnapshot does: an empty stored value
	// reports as the spec's default, so clients see consistent env
	// names whether they read the response or re-fetch via
	// GetChainStatus.
	if prev == "" {
		s.ws.stateMu.RLock()
		if s.ws.rootChain != nil && s.ws.rootChain.Spec != nil {
			prev = s.ws.rootChain.Spec.DefaultEnvironment
		}
		s.ws.stateMu.RUnlock()
	}
	return prev, nil
}

// chainSpecSnapshot is a tiny lock-free copy of the bits
// SetActiveEnvironment validates against — keeps the validation
// loop outside the workspace RLock so the (cheap) name compare
// doesn't have to be re-litigated under contention.
type chainSpecSnapshot struct {
	configured bool
	envs       []string
}
