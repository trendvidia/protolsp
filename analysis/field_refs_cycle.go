// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C4b of the chameleon-integration design: cycle detection across
// ${field.path} references. Builds on C4's recognition pass —
// collectFieldReferences gives the ${...} occurrences; this file
// folds them into a dependency graph and surfaces cycles.
//
// Scope for this slice: TOP-LEVEL cycles only. The graph nodes are
// top-level Assignment keys; edges go from the assignment to the
// first segment of each referenced path. So `a = "${b.x}"` adds an
// edge a → b. References whose first segment is `env` or `ctx` are
// runtime context interpolations and skip the graph entirely.
//
// Cycles inside nested Blocks (e.g. `db { host = "${api_url}" }`
// participating in a cycle with a top-level `api_url`) are NOT
// detected in v1 — the graph would need full-path nodes and a
// cross-depth walker. Doing so doubles the complexity for the
// uncommon case; calling it out here means we don't pretend
// otherwise.

package analysis

import (
	"fmt"
	"sort"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/interp"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// cycleReferenceDiagnostics returns one Error per top-level
// assignment that participates in a ${...} reference cycle. The
// message names the cycle path (a → b → c → a) so users see the
// ring at a glance.
//
// Silent when there's no schema bound (no descriptor to validate
// the path syntax against — same gate as the unresolvable check).
// Cycles whose edges aren't grounded on real fields don't surface;
// the unresolvable-reference diagnostic from C4 covers those.
func (s *Workspace) cycleReferenceDiagnostics(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil || doc.Parsed == nil {
		return nil
	}
	if doc.Parsed.TypeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(doc.Parsed.TypeURL))
	if err != nil || mt == nil {
		return nil
	}
	desc := mt.Descriptor()
	deps, assignPos := collectTopLevelDependencies(doc.Parsed, desc)
	if len(deps) == 0 {
		return nil
	}
	cyclePaths := findCyclePaths(deps)
	if len(cyclePaths) == 0 {
		return nil
	}
	// Sort cycle members for deterministic diagnostic order — tests
	// rely on stable output, and editor UIs render cleaner when
	// successive runs don't reshuffle.
	members := make([]string, 0, len(cyclePaths))
	for m := range cyclePaths {
		members = append(members, m)
	}
	sort.Strings(members)

	var out []protocol.Diagnostic
	for _, name := range members {
		pos, ok := assignPos[name]
		if !ok {
			continue
		}
		out = append(out, protocol.Diagnostic{
			Range:    assignmentNameRange(pos, name),
			Severity: protocol.DiagnosticSeverityError,
			Source:   optString("protolsp/schema"),
			Message: protocol.String(fmt.Sprintf(
				"field %q is part of a ${...} reference cycle: %s",
				name, strings.Join(cyclePaths[name], " → "))),
		})
	}
	return out
}

// collectTopLevelDependencies walks top-level Assignments whose
// values are StringVals, scans the strings for ${...} references,
// and returns:
//   - deps: top-level field name → list of top-level field names it
//     references (first segment of each reference path).
//   - assignPos: top-level field name → its Pos in source.
//
// Both maps are keyed by the assignment's Key, so each top-level
// field appears at most once. The descriptor argument gates which
// references count as "real field references": env / ctx prefixes
// are skipped (context interpolation), and references that don't
// resolve at all are also skipped — they're already covered by the
// unresolvable-reference diagnostic from C4, so the cycle check
// shouldn't double-fire on them.
func collectTopLevelDependencies(
	parsed *pxf.Document,
	desc protoreflect.MessageDescriptor,
) (deps map[string][]string, assignPos map[string]pxf.Position) {
	deps = map[string][]string{}
	assignPos = map[string]pxf.Position{}
	for _, e := range parsed.Entries {
		a, ok := e.(*pxf.Assignment)
		if !ok {
			continue
		}
		sv, ok := a.Value.(*pxf.StringVal)
		if !ok {
			continue
		}
		assignPos[a.Key] = a.Pos
		for _, r := range interp.FindAll(sv.Value) {
			refPath := r.Path
			if ok, isCtx := resolveFieldPath(refPath, desc); !ok || isCtx {
				continue
			}
			first := refPath
			if dot := strings.IndexByte(refPath, '.'); dot >= 0 {
				first = refPath[:dot]
			}
			// Don't double-add the same edge — cleaner cycle output.
			if !contains(deps[a.Key], first) {
				deps[a.Key] = append(deps[a.Key], first)
			}
		}
	}
	return deps, assignPos
}

// contains is a tiny helper to dedupe small slices without
// pulling slices.Contains everywhere. Top-level fields are few;
// linear scan is fine.
func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

// findCyclePaths returns, for every node that participates in any
// reference cycle, one cycle path through that node (in order, with
// the node itself appearing at start and end so the rendered
// "a → b → c → a" reads naturally). The path is computed via a
// fresh DFS from each candidate — O(V·(V+E)), bounded by the small
// per-document field count.
//
// When multiple cycles pass through a node, the first cycle DFS
// finds is returned. That's deterministic given the iteration order
// of deps (Go maps aren't sorted — callers wanting deterministic
// output sort the result themselves; cycleReferenceDiagnostics does).
func findCyclePaths(deps map[string][]string) map[string][]string {
	out := map[string][]string{}
	for start := range deps {
		if path := cyclePathThrough(start, deps); path != nil {
			out[start] = path
		}
	}
	return out
}

// cyclePathThrough returns a cycle path starting and ending at
// `start`, or nil when no cycle goes through it. Edge case: a self
// loop a → a returns ["a", "a"].
func cyclePathThrough(start string, deps map[string][]string) []string {
	stack := []string{start}
	visited := map[string]bool{start: true}
	var dfs func(node string) []string
	dfs = func(node string) []string {
		for _, next := range deps[node] {
			if next == start {
				return append(append([]string(nil), stack...), start)
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			stack = append(stack, next)
			if path := dfs(next); path != nil {
				return path
			}
			stack = stack[:len(stack)-1]
			delete(visited, next)
		}
		return nil
	}
	return dfs(start)
}

// assignmentNameRange returns an LSP range covering the
// assignment's field-name token, used for the cycle diagnostic's
// squiggle. The token starts at the assignment's Pos and runs for
// len(name) bytes — assignment names are pure ASCII identifiers,
// so byte length equals column width.
func assignmentNameRange(pos pxf.Position, name string) protocol.Range {
	start := protocol.Position{
		Line:      uint32(pos.Line - 1),   // #nosec G115 -- pxf Line is positive
		Character: uint32(pos.Column - 1), // #nosec G115 -- pxf Column is positive
	}
	end := protocol.Position{
		Line:      start.Line,
		Character: start.Character + uint32(len(name)),
	}
	return protocol.Range{Start: start, End: end}
}
