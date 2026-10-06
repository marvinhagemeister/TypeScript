# Experimental edit projection

Design sketch for [TypeScript #63879](https://github.com/microsoft/TypeScript/issues/63879).
This is an internal, opt-in prototype with experimental LSP/RPC integration, not a supported API.
The editing extension is negotiated separately from the existing content-mapper protocol.

## Boundary

TypeScript computes generated-space edits using its existing rename and organize-import implementations.
A plan captures those edits, their projections, authored documents, and one host snapshot identity.
An optional language-specific provider translates a complete owned batch. TypeScript validates coverage,
ownership, ranges, conflicts and freshness before producing one authored, versioned workspace edit.

Location fidelity and edit projection remain independent. Atom and Alias retain their existing geometry.
Providers are partial functions: rejection is a normal result, not permission to emit a subset.

## Prototype contract

- All internal ranges use half-open UTF-8 byte offsets, matching compiler text ranges. Only final LSP
  serialization converts to negotiated positions. Boundaries may not split an encoded character.
- The host supplies a nonempty snapshot ID covering the program, authored content (including closed
  documents), mapper configuration and transform identities. Its freshness callback is checked before
  and after projection. Open document versions are included in the final `documentChanges`.
- Input IDs are local to a plan. Projection IDs identify generated outputs; document IDs identify
  authored destinations. Providers cannot nominate arbitrary paths or create files.
- Each provider receives one batch per operation, including exact and non-exact edits. It returns the
  same snapshot ID and groups of covered input IDs with authored edits. Many inputs may produce one
  authored rewrite, and one input may produce several edits.
- Every input must be covered exactly once. An explicit generated-only result requires a reason and
  may cover only inputs without mapped authored content. It means regeneration needs no authored edit;
  it is a provider assertion, not a semantic guarantee proven by TypeScript.
- Responses cannot edit documents owned by another provider. Identical authored edits are deduplicated;
  overlaps, same-range/different-text replacements and ambiguous coincident insertions are rejected.
- Missing providers use exact-only projection. A required non-exact or generated edit without a provider
  rejects the whole plan, rather than returning a partial rename. Registered provider errors never fall
  back to an exact-only subset. Cancellation discards all results; providers must honor context deadlines.
- Provider calls happen after semantic computation has released checker resources. RPC calls hold no
  host/project lock and have a five-second deadline. Cancellation while waiting for a reply preserves
  the connection. A stalled write closes the transport to unblock it; that failed mapper connection
  may require a project/host reload. Direct in-process providers must still cooperate with cancellation.

## Rename

Preparation resolves an authored position to one semantic target. A whole-token Atom can select that
symbol from any character; unrelated overlapping projections are rejected. Execution repeats resolution
and eligibility checks and does not depend on `prepareRename` having run.

The prototype accepts canonical identifier input only. `next-item` is rejected, not guessed to mean
`nextItem`. The provider renders `nextItem` as `next-item` at kebab sites and as `nextItem` at camel sites.
TypeScript's complete per-occurrence replacement is retained, including shorthand and alias syntax;
providers must not blindly case-convert an entire replacement expression. Authored-input normalization
is a future optional capability at the initiating language boundary, before semantic computation.

The generated planner retains all required occurrences, independent of presentation filtering. Existing
TypeScript alias semantics still decide which references belong to the rename. A mapper is responsible
for representing all relevant authored dependencies in its generated program.

## Organize imports

Generated edits are materialized before authored conversion or formatting can discard them. The provider
owns authored layout, comment placement and helper exclusion, not TypeScript's unused-import analysis.
A structural provider may reconstruct an authored import block from the batch or reject it. Arbitrary
foreign syntax cannot be handled by merely remapping endpoints.

## Experimental mapper RPC

The host advertises `editProjectionVersion: 1` in `openProject`. A mapper opts in per project by returning:

```json
{"editProjection":{"version":1,"rename":true,"organizeImports":true}}
```

Omitting this object retains legacy behavior. Unsupported editing versions fail project opening rather
than silently selecting an incompatible contract. The value-only wire types are in
`contentmapper/edits.go`; no compiler AST or checker is passed to the process.

- `prepareRename`: receives `projectHandle`, `snapshot`, `positionEncoding: "utf-8"`, authored `fileName`,
  `content`, byte `start`/`end`, and canonical `name`. Returns the same `snapshot`, `canRename`, and an
  optional rejection `message`. This approves the initiating syntax; it does not replace TypeScript's
  semantic eligibility or execution-time resolution. Plain-TypeScript origins need no mapper preparation.
- `projectEdits`: receives `projectHandle`, `snapshot`, `positionEncoding: "utf-8"`, `operation`
  (`rename` or `organizeImports`), optional `newName`/`importAction`, and arrays of `documents`,
  `projections`, and `edits`. Documents contain IDs, authored paths/text and nullable open versions;
  projections contain IDs, document IDs, generated paths/text, transform identities and tuple-array
  mappings. Edits contain IDs, projection IDs, byte `start`/`end` and fully materialized `newText`.
- Its response echoes `snapshot` and supplies `results`: each result covers `inputs` (edit IDs) with
  authored `edits` (`document`, `start`, `end`, `newText`), or `generatedOnly: true` plus `reason`.
  IDs are opaque and operation-local, not necessarily contiguous within a provider batch. A response cannot add destination paths.

Editing always uses UTF-8 offsets, even when transform mappings or the LSP client negotiate UTF-16.
The host converts mappings before sending them, validates wire integers before narrowing to compiler
positions, and serializes only authored URIs in the client's position encoding.

## LSP integration and compatibility

The opt-in rename path loads the known solution/project tree, retains one snapshot, and reuses native
cross-project symbol traversal to collect generated edits **before** projection. Calls are batched by
mapper and transform identity. A rename initiated in a normal `.ts` file uses destination providers too.
No required provider error falls back to a partial legacy result. Legacy mappers retain their previous
editing semantics; changing partial rename globally is still a separate compatibility decision.

Snapshot capture is synchronous with request ordering; projection runs outside the LSP dispatcher so
`didChange`, configuration changes and other semantic requests can proceed during a mapper callback.
Freshness checks flush pending changes, compare actual snapshot IDs, and additionally read edited closed
files from the host filesystem. Changed host snapshots return LSP `ContentModified`. Provider snapshots
must also match, and mapper transform identities are checked before and after RPC.

Opted-in import actions replace the corresponding built-in action, rather than adding a competing one.
Identical/no-op authored rewrites are removed after validation; an empty edit produces no import action.
Other code actions retain their providers. Projected edits require client `workspaceEdit.documentChanges`
support; lacking it rejects the affected operation, not unrelated quick fixes.

This remains experimental: all-tree loading can add latency to rename in mapper-enabled sessions, even
when a particular target has no mapped references. Filtering only by the initiating extension would be
incorrect. Targeted project discovery/benchmarking, public JavaScript APIs, authored-input normalization,
resource operations, broad framework adoption and a manual editor UI smoke test remain future work.
Disabled solution searching/referenced-project loading retains the host's existing discovery limits.

Versioned workspace edits protect open buffers; they do not promise rollback or atomic application in
all clients. Closed-file races after the final freshness check still need host/client coordination;
changes to other closed semantic inputs rely on the host's existing file-watcher invalidation.

## Validation

1. Rename a property across plain TypeScript and camel/kebab authored references, starting at each
   origin. Apply edits, re-transform, and check resulting names and diagnostics.
2. Organize multiline/commented imports alongside synthesized helper imports. Preserve authored
   comments and exclude helpers without changing the TypeScript analysis.
3. Deduplicate repeated projections; reject conflicting or semantically ambiguous projections.
4. Reject stale snapshots, malformed coverage, foreign destinations, invalid/overflowing ranges,
   provider failures, cancellation and timeouts without returning any partial workspace edit.
5. Exercise framed LSP requests and a separate mapper subprocess, both UTF-8/UTF-16 clients, open/closed
   versions, cross-project solution references, legacy capability omission and import-action replacement.
6. Hold a mapper response while processing a real `didChange` and semantic hover, or change a closed file
   without a watcher event, then require whole-operation rejection. Test configuration changes too.

`go -C tsc test ./internal/lsp -run '^TestLSPProjected'` runs the protocol integration tests. Successful
rename/import tests apply authored edits and re-transform through the mapper subprocess before checking
LSP diagnostics; controlled pipe-based fixtures exercise failure and concurrency boundaries.

Tests should inspect authored output and semantic effects, not demand byte-for-byte equivalence between
regenerated scaffolding and a patched copy of the old generated text.
