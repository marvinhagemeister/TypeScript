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
- Each provider receives one batch per project context and operation, including exact and non-exact edits. It returns the
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

By default, rename accepts canonical identifier input only. A mapper can separately opt into
`renameInput` to supply an authored placeholder and normalize user input before semantic edit collection.
For example, a Vue-like mapper can show `save-item`, accept `save-foo` (or optionally `@save-foo`), and
return canonical `saveFoo`. This syntax policy belongs entirely to the mapper, not TypeScript.

Only the initiating mapper receives raw input. It normalizes once per execution, even if that input is
already a valid identifier or the editor skipped preparation. Execution resolves the current target and
uses current authored content; a previous preparation result is never cached or trusted. The normalized
result must be a non-keyword TypeScript identifier. A malformed, missing, stale, rejected or failed result
aborts the operation; it never falls back to the original input. Normalization cannot retarget the symbol
or change the trigger range, which remains governed by semantic resolution and mapping geometry.

TypeScript and every destination provider receive the canonical name. Destination providers render that
name according to each site's syntax; they do not normalize it again. Plain `.ts` origins still require
canonical input on the projected path. Mappers without `renameInput` retain canonical placeholders and
input, even if they return unnegotiated placeholder/normalization fields.

TypeScript's complete per-occurrence replacement is retained, including shorthand and alias syntax;
providers must not blindly case-convert an entire replacement expression.

The generated planner retains all required occurrences, independent of presentation filtering. Existing
TypeScript alias semantics still decide which references belong to the rename. A mapper is responsible
for representing all relevant authored dependencies in its generated program.

### Derived rename effects

A separate `derivedRename` opt-in accounts for generated spans affected by an authored replacement
but absent from TypeScript's semantic rename set. For example, a Vue listener can project to both
`saveItem` (a semantic event reference) and `onSaveItem` (a derived property).

The host retains every known projection of the affected authored documents, including views with no
native edit. It enumerates intersections without feature filtering and gives each collateral effect an
operation-local ID, projection ID, generated byte range and `inputs` listing its semantic roots. Effects
are obligations, not additional TypeScript edits. Atom/Alias geometry and navigation flags do not change.

An opted-in result acknowledges effect IDs in `derivedEffects`, alongside its normal `inputs` and authored
`edits`. Every effect must be acknowledged exactly once, by a result containing a listed root input and
an authored edit that actually intersects the effect's origin. Unknown, duplicate, foreign, unrooted or
untouched effects reject the operation. `generatedOnly` cannot acknowledge effects or discard mapped
occurrences. A required effect without a provider rejects even when the native edit maps exactly.

For opted-in providers the host recomputes impact from all returned authored edits, including wider and
supplemental replacements. Every resulting generated range must be a native edit or an acknowledged
effect in the frozen request. This first version rejects expansion outside that set; it does not retry,
extend document authority, or accept a document-wide waiver. Insertion checks include both adjacent
segments and zero-width anchors. Omission of the capability retains conservative collateral rejection.

This is structural coverage, not a proof of the mapper's language semantics. The mapper must refuse
independent bindings and correctly classify genuine derivations. Static metadata or clean diagnostics
would not prove this either. Missing semantic references must first be represented by the framework's
generated program; projection cannot discover dependencies absent from that model.

## Organize imports

Generated edits are materialized before authored conversion or formatting can discard them. The provider
owns authored layout, comment placement and helper exclusion, not TypeScript's unused-import analysis.
A structural provider may reconstruct an authored import block from the batch or reject it. Arbitrary
foreign syntax cannot be handled by merely remapping endpoints.

## Experimental mapper RPC

The host advertises `editProjectionVersion: 1` and `derivedRename: true` in `openProject`. A mapper opts in per project by returning:

```json
{"editProjection":{"version":1,"rename":true,"renameInput":true,"derivedRename":true,"organizeImports":true}}
```

Omitting this object retains legacy behavior. `renameInput` and `derivedRename` are independent optional
capabilities, each requiring `rename: true`. A mapper must check the host's `derivedRename` advertisement
before opting in; old version-1 hosts do not implement the extension.
Unsupported editing versions or inconsistent input capabilities fail project opening rather than
silently selecting an incompatible contract. The value-only wire types are in
`contentmapper/edits.go`; no compiler AST or checker is passed to the process.

- `prepareRename`: receives `projectHandle`, `snapshot`, `positionEncoding: "utf-8"`, authored `fileName`,
  `content`, byte `start`/`end`, and canonical `name`. Returns the same `snapshot`, `canRename`, and an
  optional rejection `message`. With `renameInput`, accepted responses must also provide a nonempty
  `placeholder`. During execution only, the request additionally contains the untouched user `newName`,
  and accepted responses must include a valid canonical `normalizedName`. Absence of `newName` denotes
  preparation; an explicitly empty string still denotes execution and must reach the mapper unchanged.
  This approves the initiating syntax; it does not replace TypeScript's semantic eligibility or
  execution-time resolution. Plain-TypeScript origins need no mapper preparation.
- `projectEdits`: receives `projectHandle`, `snapshot`, `positionEncoding: "utf-8"`, `operation`
  (`rename` or `organizeImports`), optional `newName`/`importAction`, and arrays of `documents`,
  `projections`, and `edits`. Documents contain IDs, authored paths/text and nullable open versions;
  projections contain IDs, document IDs, generated paths/text, transform identities and tuple-array
  mappings. Edits contain IDs, projection IDs, byte `start`/`end` and fully materialized `newText`.
  With `derivedRename`, optional `effects` contain `id`, `projection`, byte `start`/`end`, and root `inputs`.
- Its response echoes `snapshot` and supplies `results`: each result covers `inputs` (edit IDs) with
  authored `edits` (`document`, `start`, `end`, `newText`), or `generatedOnly: true` plus `reason`.
  With `derivedRename`, results can additionally acknowledge `derivedEffects` (effect IDs). Edit IDs and
  effect IDs have separate namespaces. IDs are opaque and operation-local, not necessarily contiguous
  within a provider batch. A response cannot add destination paths.

Editing always uses UTF-8 offsets, even when transform mappings or the LSP client negotiate UTF-16.
The host converts mappings before sending them, validates wire integers before narrowing to compiler
positions, and serializes only authored URIs in the client's position encoding.

## LSP integration and compatibility

The opt-in rename path loads the known solution/project tree, retains one snapshot, and reuses native
cross-project symbol traversal to collect generated edits **before** projection. Calls are batched by
mapper, project context and transform identity. Equal transform output does not make project handles
interchangeable. This bounded implementation rejects an affected mapped document shared by distinct
editing contexts rather than selecting an arbitrary provider or merging unverified context-specific
responses. A rename initiated in a normal `.ts` file uses destination providers too.
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
incorrect. Targeted project discovery/benchmarking, public JavaScript APIs, resource operations, broad
framework adoption and a manual editor UI smoke test remain future work.
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
7. Exercise authored placeholders and input through the real mapper process, with/without preparation,
   and normalize before cross-project collection. Verify arbitrary mapper policies, exactly-once origin
   normalization, canonical-only compatibility, malformed responses, empty wire input, cancellation,
   stale snapshots and timeouts before any destination projection occurs.

8. Account for a Vue-shaped JSDoc event reference and derived listener property, without changing
   navigation results or feature masks. Reject missing/duplicate/unrooted effects, independent bindings,
   widened edits, edit-free views, context conflicts and unsafe exact fallback.

`go -C tsc test ./internal/lsp -run 'TestLSPProjected|TestLSPDerived|TestLSPRenameBatches|TestDerivedRenameDoesNotChangeNavigation'` runs the protocol integration tests. Successful
rename/import tests apply authored edits and re-transform through the mapper subprocess before checking
LSP diagnostics; controlled pipe-based fixtures exercise failure and concurrency boundaries.

Tests should inspect authored output and semantic effects, not demand byte-for-byte equivalence between
regenerated scaffolding and a patched copy of the old generated text.
