# Requirements Enforcement Modes for the Artifact Operator

**Status:** Approved for planning
**Date:** 2026-07-16

## Problem

The artifact operator's Plugin and Rulesfile reconcilers check OCI-declared requirements
(`plugin_api_version`, engine version, plugin-to-plugin dependencies) against the running
Falco instance's live `/versions` capabilities before installing an artifact. This check is
gated by a CLI flag currently named `--strict` (default `false`).

Three problems with the current implementation:

1. **The flag name doesn't describe what it does.** `--strict` says nothing about what
   becomes strict.
2. **Non-strict mode doesn't actually behave differently for a known-unsatisfied
   requirement.** Today, `enforcePluginCompatibility`/`enforceRulesfileCompatibility` return
   `skip=true` (blocking install) for a *known* unsatisfied requirement in both modes —
   `strict` only changes behavior for the *unknown* cases (`ArtifactMeta` not yet fetched,
   Falco `/versions` unreachable). There is no way to say "install anyway, just warn me."
3. **Rulesfile evicts a working install on any blocked reconcile.** `RulesfileReconciler`
   calls `evictInstalledArtifacts` unconditionally whenever a compatibility check fails
   (`controllers/artifact/rulesfile/controller.go:171-180`), deleting every installed file
   and clearing their conditions. If a user bumps an OCI tag to a version that turns out to
   be incompatible, this removes the rulesfile that was previously working — a bad update can
   silently regress Falco's actual security coverage. Plugin has no equivalent eviction logic
   today (it simply never runs `ensurePlugin` when blocked), which is the correct behavior —
   Rulesfile is the outlier.

## Goals

- Rename the flag to something self-describing: `--enforce-requirements`.
- Make enforcement the **default** (opt-out), since this is a security-relevant control and
  the safer default is not to install something whose compatibility is unverified or known
  bad.
- In **advise mode** (`--enforce-requirements=false`): proceed with installing/configuring the
  artifact even when requirements are unmet or unknown, but make that fact visible in both
  status conditions and logs/events.
- In **enforce mode** (default): keep blocking installation when requirements are unmet or
  unknown, as today.
- In both modes: if a *previously installed* artifact was working and a *subsequent update*
  (e.g. an OCI tag bump) is incompatible, never remove or replace the working install. State
  in the status that the update was rejected/not applied, without regressing coverage.
- Add chainsaw e2e coverage for advise mode, enforce mode, and the update-rejection scenario,
  for both Plugin and Rulesfile.

## Non-goals

- No change to how `ArtifactMeta`/`Requirements`/`Dependencies` are populated (still owned by
  the instance operator, out of scope here).
- No change to the semver comparison helpers (`SemverAtLeast`, `SemverMajorCompatible`).
- No new CRD fields or new condition *types* — this reuses the existing
  `DependenciesSatisfied` condition type with a richer Reason/message vocabulary.

## Design

### 1. Flag rename and default

`--strict bool` (default `false`) becomes `--enforce-requirements bool` (default `true`).
Both reconcilers' constructor parameter and struct field rename from `strict` to
`enforceRequirements`. Passing `false` now means "advise mode" — the same permissive
installation behavior the old `strict=false` already had for the *unknown* cases, extended to
also apply to the *known-unsatisfied* case (see below).

### 2. Decouple `Programmed` from `DependenciesSatisfied`

Both reconcilers derive the gateway `Programmed` condition as "AND over every other condition
present on the node object" (`controllers/artifact/plugin/controller.go:150-176`,
`controllers/artifact/rulesfile/controller.go:137-163`). Today `DependenciesSatisfied=False`
is included in that AND, so a blocked reconcile always yields `Programmed=False` — correct
today, but incompatible with advise mode's "install anyway" requirement, where something is
genuinely programmed on disk despite an unmet requirement.

Fix: exclude `DependenciesSatisfied` from the aggregation loop, alongside the existing
exclusion of `Programmed` itself. Effects, traced against the actual control flow:

- **Advise mode, unsatisfied**: `ensurePlugin`/`ensureRulesfile` still run and set
  `OCIArtifactProgrammed=True` etc. → `Programmed=True` even though
  `DependenciesSatisfied=False`.
- **Enforce mode, blocked, nothing ever installed**: `ensurePlugin`/`ensureRulesfile` never
  run, no per-source condition is ever set, the aggregation loop sees zero conditions →
  `Programmed=False` (unchanged from today).
- **Enforce mode, blocked update, something already installed**: the per-source condition
  from the *last successful* reconcile (e.g. `OCIArtifactProgrammed=True`) is still present on
  the node object — this reconcile skips before touching it — so the aggregation loop still
  sees it and `Programmed` stays `True`. This is correct: something valid is still active.

No new condition type is introduced.

### 3. Reason/message vocabulary

A single shared helper, `artifact.DependenciesNotSatisfiedOutcome(enforceRequirements bool,
alreadyInstalled bool, baseMsg string) (skip bool, reason string, message string)`, centralizes
the mode/installed-state branching in one place (`internal/pkg/artifact`, importable by both
reconcilers) rather than duplicating the branch at every call site:

| enforceRequirements | alreadyInstalled | skip  | Reason                                          |
|---|---|---|---|
| `false` | (any) | `false` | `ReasonDependenciesNotSatisfiedInstalledAnyway` |
| `true`  | `true`  | `true`  | `ReasonDependenciesNotSatisfiedUpdateRejected`  |
| `true`  | `false` | `true`  | `ReasonDependenciesNotSatisfied` (existing, unchanged meaning: nothing was ever installed) |

The message is the existing per-case `baseMsg` (unchanged — e.g. "Plugin requires
plugin_api_version 3.0.0 but Falco reports 2.0.0: major versions are incompatible") with a
mode-specific suffix appended: `MessageSuffixInstalledAnyway` or `MessageSuffixUpdateRejected`.
This avoids duplicating every existing message format per mode.

`alreadyInstalled` is computed by the caller: for Plugin, `artifact.FindInstalled(nodeObj.Status.InstalledArtifacts,
artifact.MediumOCI) != nil` (only OCI-medium plugins carry requirements); for Rulesfile,
`len(nodeObj.Status.InstalledArtifacts) > 0` (requirements apply to the Rulesfile as a whole,
independent of which medium(s) are configured).

A `Warning` Event (`artifact.RecordWarning`, already used throughout both controllers) is
emitted on both the advise-installed-anyway and update-rejected outcomes so the fact is
visible in `kubectl describe`/controller logs immediately, not just in status.

### 4. Reconciler changes

- **Plugin `enforcePluginCompatibility`**: at both failure points (capability missing, semver
  not satisfied — `controllers/artifact/plugin/controller.go:530-572`), replace the
  unconditional `return true, nil` with a call to `DependenciesNotSatisfiedOutcome` and use its
  returned `skip`/reason/message.
- **Rulesfile `checkEngineRequirement`** (`controllers/artifact/rulesfile/controller.go:695-743`)
  and the **plugin-dependency loop** in `enforceRulesfileCompatibility`
  (`controllers/artifact/rulesfile/controller.go:660-687`): same replacement at each of their
  three failure points.
- **Rulesfile `Reconcile`**: delete the `evictInstalledArtifacts` call in the `skip` branch
  (`controllers/artifact/rulesfile/controller.go:171-180`) and the now-dead
  `evictInstalledArtifacts` method (`controllers/artifact/rulesfile/controller.go:212-228`).
  Rulesfile now behaves like Plugin already does: a blocked reconcile simply doesn't touch
  disk, leaving any previously-installed artifact exactly as it was.

### 5. Update-safety interaction with priority changes

Traced through `LocalStore.Store` (`internal/pkg/artifact/store.go:86-147`): while blocked,
`nodeObj.Status.InstalledArtifacts` (and `FindInstalled`, which reads it) is never touched, so
it keeps pointing at the exact file that is actually live on disk. When a later reconcile is
unblocked (Falco upgraded, or the spec was fixed) — even if `priority` also changed in the
same spec update — `Store` computes `newPath` from whatever the *current* desired spec says
and does a plain `current.Path != newPath` comparison (not a per-field diff): if they differ,
it writes the new file, then removes the old one (`StoreActionPriorityChanged`). This already
works correctly with no code change, for Rulesfile/Config paths (which encode priority in the
filename). Plugin `.so` paths never include priority, so this scenario doesn't apply to
Plugin.

The only real gap is test coverage: there is no `store_test.go` for `LocalStore.Store` at all,
and no chainsaw test changes `spec.priority` on an existing Rulesfile. Both are added (see
Testing below) to lock in this already-correct behavior.

## Testing

- **Unit — `internal/pkg/artifact/store_test.go` (new file)**: `LocalStore.Store` with a
  `current` File at one path and a call requesting a different priority (same content hash) →
  assert old path removed from the mock FS, new path present, `StoreActionPriorityChanged`
  returned.
- **Unit — both controllers' existing compatibility tests** (`TestEnforcePluginCompatibility`,
  `TestEnforceRulesfileCompatibility`, `TestCheckEngineRequirement`): rename the `strict` test
  field/reconciler field to `enforceRequirements`; add cases for advise-mode
  (`enforceRequirements=false`, unsatisfied requirement) asserting `skip=false` and
  `ReasonDependenciesNotSatisfiedInstalledAnyway`, and for update-rejected (`enforceRequirements=true`,
  unsatisfied requirement, pre-populated `InstalledArtifacts`) asserting `skip=true` and
  `ReasonDependenciesNotSatisfiedUpdateRejected`.
- **Unit — both controllers' `TestReconcile`**: new case exercising the full path — advise
  mode with an unsatisfied requirement still results in `Programmed=True` and the artifact
  actually stored.
- **Unit — `internal/pkg/artifact` new helper test**: table test over
  `DependenciesNotSatisfiedOutcome`'s three branches.
- **Chainsaw e2e**, extending the existing mock-`/versions` infrastructure
  (`test/e2e/chainsaw/artifact-operator/plugin-requirements`):
  - Plugin advise-mode: unsatisfied `plugin_api_version`, `--enforce-requirements=false` →
    `.so`/config land on disk, `Programmed=True`, `DependenciesSatisfied=False` reason
    `InstalledAnyway`.
  - Plugin update-rejected: install a satisfied plugin, then mock Falco's capability down to
    incompatible and bump the Plugin's OCI tag → old `.so`/config remain, `Programmed` stays
    `True`, `DependenciesSatisfied=False` reason `UpdateRejected`.
  - Rulesfile advise-mode and update-rejected mirrors (new mock-versions-based Rulesfile test,
    since only Plugin has one today).
  - Existing `plugin-requirements` test: drop its now-redundant `"--strict"` container arg
    (enforce is the new default).

## Open items resolved during brainstorming

- Programmed/DependenciesSatisfied decoupling: confirmed safe by tracing the actual
  aggregation loop and control flow (see §2).
- Blocked-update messaging: confirmed as a design requirement (see §3), not left generic.
- Priority-change safety during a blocked/rejected update: confirmed already correct by
  tracing `LocalStore.Store`; only test coverage was missing (see §5, Testing).
