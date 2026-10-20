# Default Dictation Splitting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable the existing silence-based dictation splitter by default with a 10-second minimum segment and a 1-second silence threshold.

**Architecture:** Keep the current growing-WAV upload, energy-based silence detection, ordered transcript joining, and one final transform. Change the defaults and their documentation together. Under the compatibility policy below, accept positive finite split thresholds independently of recording duration and compare elapsed frame durations without converting those thresholds to integers.

**Tech Stack:** Go 1.25, existing TOML parser, existing WAV/splitter helpers and fake gateway; no new dependencies.

**Spec:** The conversation's confirmed project-default scope and the requirements below. There is no separate design document for this bounded change.

**Status:** Execution requested on 2026-10-19. The user selected a one-second pause instead of the draft's 0.75 seconds. Keep short recordings valid under the compatibility policy below.

## Requirements and compatibility decision

- Set `[split] enabled = true`, `after = 10`, and `pause = 1` in application defaults and bundled examples.
- `after` is a minimum duration, not a periodic cut or a maximum request duration. A qualifying silence is still required.
- Preserve explicitly configured values, including `enabled = false`. Missing values inherit defaults. Existing files are not rewritten; an existing explicit opt-out remains an opt-out.
- Keep transform enabled and applied once to the ordered, joined transcripts. Keep whole-file fallback and cancellation behavior.
- Preserve `trim_below_db = 0` disabling silence-based splitting.
- **Compatibility policy:** Accept positive finite `after` and `pause` values even when they exceed `general.max_seconds`. A recording that never reaches a threshold finishes normally without a split. Do not clamp user values or silently alter their configuration.

## Global Constraints

- Always `GOTOOLCHAIN=local`; never auto-download Go 1.26.
- Prefix shell commands with `rtk`; use `rtk proxy` for raw commands.
- No realtime in `listen`; no Gemini smart; no removal or bypass of transform.
- No new codec, dependency, configuration key, gateway timeout workaround, or personal configuration change.
- Public documentation is English-only and must not contain internal endpoints, internal credential commands, or secrets.
- Preserve unrelated changes. `internal/listen/stream_test.go` already contains uncommitted live-latency test repairs; do not stage those hunks as part of this change.
- Keep the implementation coherent. Subsequent user instruction authorizes committing, pushing, and publishing the next patch release after verification and review.

## Evidence and limits

Thirty-six live-gateway measurements kept transform enabled. On the 37-second mixed-pause passage, median release-to-delivery times were 2.38 s with `after = 30`, 1.30 s with `after = 10`, and 1.23 s with `after = 5` (all at `pause = 1.5`, three runs each). On a separate 32.68-second passage containing only 0.9-second pauses, `after = 10, pause = 0.75` took 1.28 s versus 2.86 s at `pause = 1.5` (two runs each). Cutting at every qualifying short pause took 1.25 s but used seven STT requests instead of three.

The tested gateway sometimes ended unfinished uploads around 30 seconds with an empty HTTP 200 response; fallback completed those dictations. This is an observation about that gateway, not a universal API limit. Lower thresholds cannot guarantee shorter requests when speech has no qualifying pauses. Synthetic English clips and small sample counts do not establish microphone/noise/accent quality. Keep timing numbers out of deterministic tests and avoid promising universal latency savings in public docs.

## File map

| File | Responsibility in this change |
|---|---|
| `internal/config/config.go` | Default TOML; positive-finite split validation and error text |
| `internal/config/config_test.go` | First-run/inherited defaults, explicit overrides, short recording limits, invalid values |
| `internal/listen/upload.go` | Overflow-safe threshold comparisons; otherwise retain the upload pipeline |
| `internal/listen/stream_test.go` | Default splitting, fractional silence boundaries, opt-outs, joined transform behavior |
| `internal/listen/listen_test.go` | Reuse `TestMaxSecondsStopsOnItsOwn` unchanged under the compatibility policy |
| `README.md` | Explain defaults, silence-triggered cuts, opt-out, and existing-file behavior |
| `examples/config.toml` | Match the new default split values |
| `plugins/habla/skills/habla/references/configuration.md` | Match example and validation/default documentation |
| `internal/release/example_test.go`, `internal/release/plugin_test.go` | Reuse parser-backed checks that shipped examples match application defaults |
| `CHANGELOG.md` | Add an Unreleased change entry; preserve concurrent release edits |

## Review Focus

1. A recording limit below ten seconds must still load and finish: configuration cases plus `TestMaxSecondsStopsOnItsOwn`.
2. Explicit opt-out and disabled trimming must preserve a single upload: parser override cases and splitter/integration cases.
3. Short pauses or an insufficient segment length must not cut speech: existing splitter cases plus a fractional 0.75-second boundary case.
4. Relaxed validation must not turn enormous finite thresholds into negative frame counts: invalid-value cases and a large-finite-threshold no-cut case.
5. Multiple transcripts must remain ordered, receive one transform, and retain fallback behavior: the default-split integration, `TestJoinedPieces`, and `TestFailedStreamGetsOneNormalUpload`.

---

### Task 1: Ship default silence-based splitting consistently

**Files:** Modify the files listed above as needed; reuse the recording-limit and fallback tests without unrelated rewrites. No new production files.

**Interfaces:**
- Consumes: `config.Defaults() *config.Config`, `config.Load() (*config.Config, error)`, `parse(string) (*Config, error)`, and `config.Split{Enabled bool, After float64, Pause float64}`.
- Consumes: `newSplitter(*config.Config) *splitter`, `(*splitter).start(int)`, `(*splitter).cut([]byte) int`, and existing `testenv`/listen test helpers.
- Produces: the same public interfaces with the approved defaults and chosen validation policy; no changes to `startUpload`, `finish`, `joined`, or the transform interface.

- [x] **Step 1: Extend configuration coverage before changing defaults.** In `internal/config/config_test.go`, extend `TestFirstRunWritesTheDefaultFile` to check `Split{Enabled: true, After: 10, Pause: 1}`. Add table cases to `TestSplitValidation` for omitted `[split]`, `max_seconds = 1` and `5`, explicit `enabled = false`, and explicit `after = 5, pause = 1.5`; assert inherited and explicit values exactly. Under the compatibility policy, both `after = 20` and `pause = 20` with `max_seconds = 5` load unchanged. Reject zero, negative, `nan`, `inf`, and `-inf` values for each enabled threshold. Update the existing exact-error expectation in `TestInvalidFileIsAnErrorNamingTheLineAndIsUntouched` (the existing `split.after` case) to the new message: `a finite number of seconds > 0`.

- [x] **Step 2: Add behavioral coverage using existing helpers.** Add `TestDefaultSplitUpload` in `internal/listen/stream_test.go`. Build PCM from five copies of `testenv.HelloPCM(t)`, 0.66 s of zero PCM, and one final copy. The first five clips occupy ten seconds; their last 0.54 s plus the inserted silence creates a 1.2 s pause. Reuse the recorder/gateway flow from `TestLongPauseSplitsTheUpload`, with no `[split]` override for the default case. Require two STT requests before stopping, streamed WAV headers on both, then exactly one chat request containing the two transcripts in order and one final transformed copy. Table cases with `enabled = false` and `trim_below_db = 0` must make one STT request and one chat request.

  Extend the pure splitter tests with continuously voiced frames followed by silence: a 1.2 s pause before the ten-second minimum produces no cut; after the minimum, 0.98 s of silence produces no cut and 1 s does. For a custom 0.75-second threshold, 0.74 s of silence produces no cut and 0.76 s does. Use the existing 20 ms frame size and `audio.Trim` for expected padded cut positions. Under the compatibility policy, `after = 1e300` or `pause = 1e300` must also produce no cut. Keep existing ordered-join, disabled-trim, and failure assertions.

- [x] **Step 3: Run the focused tests and confirm the expected failures.**

  ```bash
  rtk proxy env GOTOOLCHAIN=local timeout 300 go test ./internal/config/ ./internal/listen/ -run 'TestFirstRunWritesTheDefaultFile|TestSplitValidation|TestDefaultSplitUpload|TestSplitter' -count=1
  ```

  Expect failures for old defaults/validation and fractional or oversized threshold behavior. Verify failures concern the intended behavior rather than fixture construction or a missing service.

- [x] **Step 4: Make the minimal production changes.** Update `DefaultTOML` to `enabled = true`, `after = 10`, `pause = 1`. Under the compatibility policy, replace the `max_seconds` bound in `decode` with positive-finite validation (`v > 0 && !math.IsInf(v, 0)` also rejects NaN), and update both error messages. In `splitter`, retain `after` and `pause` as `float64` seconds; `newSplitter` copies the configured values. In `cut`, compare the existing frame-count differences, converted to elapsed seconds using `float64(frameCount) * (float64(audio.FrameBytes) / (2 * audio.Rate))`, against those thresholds. Preserve existing start/scanned indices, padding, holdback, silence detection, cut guards, cancellation, and fallback. This avoids overflow and waits for the first full frame meeting 0.75 s rather than truncating it to 0.74 s.

- [x] **Step 5: Synchronize shipped documentation and examples.** Update both TOML examples and README to the exact new defaults. Explain that missing keys inherit defaults, explicit settings survive, `enabled = false` opts out, `trim_below_db = 0` disables splitting, and transform runs once after joining. Under the compatibility policy, replace the plugin reference's `<= max_seconds` constraint with positive-finite validation and explain that unreachable thresholds simply do not trigger. Extend the two existing release checks to compare each parsed example's `Split` with `config.Defaults().Split`. Add an Unreleased changelog entry without changing released sections or version numbers.

- [x] **Step 6: Verify the complete change.** Format touched Go files, then run:

  ```bash
  rtk proxy env GOTOOLCHAIN=local timeout 300 go test ./...
  rtk proxy env GOTOOLCHAIN=local go vet ./...
  rtk proxy env GOTOOLCHAIN=local timeout 300 go test -race ./internal/listen/ -run 'TestDefaultSplitUpload|TestLongPauseSplitsTheUpload|TestJoinedPieces|TestFailedStreamGetsOneNormalUpload|TestMaxSecondsStopsOnItsOwn' -count=1
  rtk git diff --check
  ```

  Expect all checks to pass, including the unchanged one-second recording-limit test. Search README, examples, the plugin reference, and `DefaultTOML` for stale 30/1.5 defaults or claims that splitting is disabled by default. Do not alter historical changelog entries or intentional non-default test values. The user-selected one-second pause was not a configuration in the prior live benchmark; validate it with deterministic default/boundary tests and do not attribute the 0.75-second measurements to it. The fractional-boundary correction for custom settings is at most one 20 ms frame.

- [x] **Step 7: Review and commit only this change.** Inspect `git status`, the staged diff, and recent commit style. Stage only this task's hunks, preserving the existing live-latency repairs in `stream_test.go`. Commit with `perf(listen): enable pause-based splitting by default`. After the independent review, prepare the next patch changelog section and annotated tag, push, and verify release publication as subsequently requested by the user.

## Execution handoff

The user invoked executing-plans: implement inline in an isolated worktree, with one independent review at the end. Commit the coherent change, then complete the subsequently authorized push and patch release.
