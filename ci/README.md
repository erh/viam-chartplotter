# Parked CI workflow — needs a one-step move to activate

`mobile-flutter.yml` here is the **Flutter CI workflow**, parked in `ci/`
instead of `.github/workflows/` because the automation that authored it lacks
GitHub's **`workflow`** OAuth scope and cannot push files under
`.github/workflows/`. GitHub only *runs* workflows from `.github/workflows/`, so
move it there to activate:

```bash
git mv ci/mobile-flutter.yml .github/workflows/mobile-flutter.yml
git commit -m "ci: activate Flutter workflow"
git push
```

(or via the GitHub web UI: open `ci/mobile-flutter.yml` → Raw → copy → **Add
file → Create new file** at `.github/workflows/mobile-flutter.yml` → paste →
commit.)

Once it's under `.github/workflows/` on the default branch, it runs on any
push/PR touching `mobile/**` — including the mobile-app PR after it merges
`main`. It's parked at the repo root (not under `mobile/`) on purpose, so it
doesn't make the `mobile/` directory "exist" and trip the SessionStart hook's
guard before the actual app lands.
