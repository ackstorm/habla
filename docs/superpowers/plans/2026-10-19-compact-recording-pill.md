# Compact recording pill implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Implement the approved Paper light floating bar with Orbit phase colours and a right-aligned recording duration.

**Architecture:** Replace the existing Cairo rendering in place. Keep the recordingView interface, timer sampling, window placement, input transparency and cancellation capabilities. Reuse the notification fallback.

**Tech Stack:** Go 1.25, existing GTK/Cairo bindings and Xvfb GUI tests; no new dependencies.

**Spec:** User-approved compact design, recorded below. Inline execution explicitly authorized after the design review.

## Global constraints / approved design

- White 460 × 56 px body, 12 px corners, pale border and subtle shadow (4 px outside the body).
- Orbit lavender (#aaa6ff) listening waveform, aqua (#8bdddb) transcription activity, gold (#f3cd83) refinement activity. Darker text: #6551ae, #167875, #895d14.
- A 24 px icon tile, 80 px meter, two 1 × 26 px separators (#b5c4d3 at 25% opacity), with 10 px whitespace on each side.
- Listening / Release to transcribe…; Transcribing / Turning your words into text…; Refining text / Corrections and style.
- Pulsing rose recording dot, rotating transcription spinner, shimmering four-point refinement star; moving indicator on the processing track.
- ESC / to cancel in every cancellable X11 phase. Preserve tray cancellation where Escape is unavailable, and no cancellation affordance after clipboard delivery starts. Non-held recording must describe tray stop correctly.
- One m:ss recording duration immediately before the right separator; advances only while recording. No phase/total-process timers or new configuration.
- Preserve the previously approved quiet-speech meter curve and real-level clipping colours.
- Preserve click-through/focus behavior, scaling, error persistence, notification fallback and existing hotkey behavior.
- English text, GOTOOLCHAIN=local, preserve unrelated stream_test.go changes. No release or push in this task.

## Review focus

- Large font metrics and long recording durations must not collide with the timer or cancel control.
- Tray-started recording and Wayland portal sessions must not advertise an unavailable Escape action.
- Clipboard delivery must keep the bar stable without advertising cancellation after ownership is released.
- Copy failure and subsequent recording must reset the visual state correctly.
- HiDPI and non-composited X11 must keep the bar legible and correctly placed; real Wayland needs a host check.

### Task 1: Render and verify the approved compact bar

**Files:** Modify internal/tray/pill.go, internal/gtk/gtk.go (one Cairo line binding if needed), internal/tray/pill_gui_test.go, README.md, CHANGELOG.md and docs/images/recording-pill.png. Add presentation tests in internal/tray/logic_test.go only if a pure helper is needed.

**Interfaces:** Consume recordingView.show(phase string, hint []Segment), PillHint and existing runtime state; keep those interfaces unchanged.

- [x] Update GUI assertions for a 468 × 64 window (including shadow), white body, rose dot, lavender audio meter, aqua/gold activity, subtle separators, stable geometry and frozen timer at the right. Check timer pixels contain ink and remain unchanged through both processing phases. Check Escape disappears for clipboard delivery. Keep cancellation, fallback, clipping and scale tests.
- [x] Run `HABLA_GUI=1 GOTOOLCHAIN=local timeout 300 go test ./internal/tray/ -run 'TestPillGUI$' -count=1`: expect FAIL on old dimensions/colours.
- [x] Replace the Cairo drawing/text layout, using measured minimum widths to accommodate font metrics and long timers. Keep the default body 460 × 56. Add a pure presentation helper only to map existing hint capabilities into the new title/subtitle/control copy; cover held, tray, portal, delivery and failure cases. Retain the notification body and update phase titles to match.
- [x] Run focused GUI tests, full `go test ./...`, `go vet ./...` and static build with GOTOOLCHAIN=local. Expect PASS. Inspect a native screenshot and update the README image and description.
- [x] Commit the completed change on feat/compact-recording-pill. Run task-done with the full suite and request one fresh whole-change review. Fix important findings with a regression check; leave the result ready for the user without pushing or releasing.
