# internal/cli wiring

Owner: WP-CF-configfiles.

`cmd/airview` dispatches before any GUI/console setup or instance lock:

```go
if cli.IsConfigCommand(os.Args[1:]) {
    os.Exit(cli.RunConfig(os.Args[2:], cli.OSEnv()))
}
```

On Windows GUI builds the console must be attached for output (same treatment as `-version`/`-headless`).

| Command | Exit status |
|---|---|
| `airview config check [-data d] [-config f] [-no-config] [-no-env] [-set p=v]... [-json] [-q]` | 0 valid (warnings allowed), 1 errors, 2 usage |
| `airview config print [same source flags] [-sources] [-json]` | 0, 1 if the operator file is unusable |
| `airview config example [-o file]` (refuses to overwrite) | 0 / 1 |
| `airview config env [-json]` | 0 |

The data directory is found read-only: `-data`, `$AIRVIEW_DATA`, `data` next to the executable if it exists, the user config dir (`AirView`) if it exists, else `data` next to the executable (missing = defaults). Nothing is created or rewritten.
