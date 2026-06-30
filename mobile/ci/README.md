# CI workflow (manual activation needed)

`mobile-flutter.yml` here is the Flutter CI workflow, but it's parked in
`mobile/ci/` instead of `.github/workflows/` because the automation that
committed it lacked the GitHub **`workflow`** scope required to write under
`.github/workflows/`. GitHub only *runs* workflows from `.github/workflows/`, so
one manual move activates it:

**Option A — GitHub web UI (no special scope needed):**
1. Open this file on GitHub → **Raw** → copy the contents.
2. **Add file → Create new file**, path `.github/workflows/mobile-flutter.yml`,
   paste, commit to this branch.

**Option B — local git (needs a token/SSH with `workflow` scope):**
```bash
git mv mobile/ci/mobile-flutter.yml .github/workflows/mobile-flutter.yml
git commit -m "ci: activate Flutter workflow"
git push
```

Once it's under `.github/workflows/`, it runs on any push/PR touching `mobile/**`
(see the comments in the workflow for what each job does).
