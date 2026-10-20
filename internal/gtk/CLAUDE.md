# internal/gtk

purego (dlopen, no cgo) bindings for `habla tray`: GTK 3, GDK, GLib/GObject, cairo, Ayatana, gtk-layer-shell.

## Conventions

- Bindings are exported func vars (`MenuNew`, `CairoArc`, ...), filled by `Load()` from the `libs` table (C symbol →
  `&var`). Add a function = one var + one table entry. Objects are plain `uintptr`; gboolean is `bool`.
- `Load()` runs once. Missing required lib or symbol → error naming the Debian package. gtk-layer-shell is optional:
  any failure there only makes `HasLayerShell()` false.
- Exactly four `purego.NewCallback`s, made in `Load` (`signalCB`, `drawCB`, `sourceCB`, `destroyCB`). Every
  `Connect`/`ConnectDraw`/`Idle`/`Timeout` stores its Go func in `handles` and passes the key as user_data; the
  GClosureNotify / GDestroyNotify (`destroyCB`) deletes it, so destroying a widget or removing a source frees it.
  Never call `purego.NewCallback` per closure: ~2000 per process, never freed.
- Never bind a C function with a Go func parameter type (`func(...)` in the var's signature): purego makes a new
  callback on every call and leaks it. Callback parameters are `uintptr`, passed one of the four shared callbacks.
- Everything runs on the GTK thread (the main OS thread that calls `Main`), except `Idle`, which any goroutine may
  call (g_idle_add is thread-safe). The loop's package (`internal/tray`) does `runtime.LockOSThread()` in `init`.

## Gotchas

- Importing purego makes the binary dynamically linked (libc.so.6, libdl.so.2, libpthread.so.0; no versioned glibc
  symbols), even with `CGO_ENABLED=0`: dlopen needs ld.so. Measured: `habla listen --status` 5.0 → 6.3 ms per exec.
  This package alone does not change `habla` until `main` imports it.
- Tests: `go test` runs tests off the main thread. `gtk_test.go` locks the main goroutine in `init`, runs `m.Run()`
  on a goroutine, and the main goroutine executes what tests send through `onMain`. Never `t.Fatal` inside `onMain`
  (it would `Goexit` the main goroutine): `t.Error` and return.
- GUI test: `HABLA_GUI=1 GOTOOLCHAIN=local timeout 300 go test ./internal/gtk/ -v` (also with `CGO_ENABLED=0`,
  the fakecgo path the release binary uses; the default `go test` uses real cgo when gcc is present).
  `testenv.Xvfb(t)` uses `-displayfd`, so the server picks a free display and reports it once ready.
- Debian 13 renamed the packages (`libgtk-3-0t64`, `libglib2.0-0t64`), but they `Provide` the old names, so
  `apt install libgtk-3-0` works: the error messages use the old names.
- Ayatana with no session bus (or no StatusNotifierWatcher) still returns an indicator; it only warns.
- `gtk_widget_destroy` on a menu frees its handlers even while the indicator holds a ref (dispose disconnects them).
