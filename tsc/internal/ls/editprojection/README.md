# Experimental edit projection

Design sketch for [TypeScript #63879](https://github.com/microsoft/TypeScript/issues/63879).
This is an internal, opt-in prototype, not a supported API or a change to content-mapper protocol v1.

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
- Provider calls happen after semantic computation has released checker resources. The in-process
  adapter cannot forcibly terminate a provider that ignores cancellation; an RPC host needs bounded waits.

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

## Compatibility and scope

Existing native LSP entry points and protocol-v1 mappers are unchanged. The new internal planner and
projector are explicitly invoked by tests. Within this path, completeness is mandatory. Changing the
legacy partial-rename behavior globally should be a separate compatibility decision, not hidden here.

The first slice exercises one compiler program containing normal and mapped files, including rename
initiated in a normal `.ts` file. Plans can contain multiple generated projections and providers.
Cross-project collection, editor registration/action ownership, mapper RPC transport, public JavaScript
API exposure, authored-input normalization and resource operations are not implemented by this slice.
A production adapter must gather a complete cross-project plan before projection and verify consistent
snapshot identities. It must suppress its built-in action when a specialized provider owns that action.

Versioned workspace edits protect open buffers; they do not promise rollback or atomic application in
all clients. Closed-file races after the final freshness check need host/client coordination.

## Validation

1. Rename a property across plain TypeScript and camel/kebab authored references, starting at each
   origin. Apply edits, re-transform, and check resulting names and diagnostics.
2. Organize multiline/commented imports alongside synthesized helper imports. Preserve authored
   comments and exclude helpers without changing the TypeScript analysis.
3. Deduplicate repeated projections; reject conflicting or semantically ambiguous projections.
4. Reject stale snapshots, malformed coverage, foreign destinations, invalid ranges, provider failures
   and cancellation without returning any partial workspace edit.

Tests should inspect authored output and semantic effects, not demand byte-for-byte equivalence between
regenerated scaffolding and a patched copy of the old generated text.
