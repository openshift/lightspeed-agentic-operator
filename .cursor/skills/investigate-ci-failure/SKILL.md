---
name: investigate-ci-failure
description: Investigate CI/Prow job failures on a GitHub pull request. Use when the user pastes a PR URL and asks about CI failures, red checks, test failures, or wants to understand why a job failed.
disable-model-invocation: true
---

# Investigate CI Failure

Given a PR URL (e.g. `https://github.com/openshift/lightspeed-agentic-operator/pull/123`), diagnose why CI jobs failed.

## Workflow

### 1. Extract PR info

Parse org, repo, and PR number from the URL. Fetch metadata with `gh`:

```bash
# PR metadata
gh api repos/{org}/{repo}/pulls/{pr} --jq '{title, state, user: .user.login, head_sha: .head.sha}'

# Changed files
gh api repos/{org}/{repo}/pulls/{pr}/files --jq '.[].filename'
```

### 2. Get check statuses

```bash
# All checks at a glance
gh pr checks {pr} --repo {org}/{repo}

# Detailed statuses with Prow URLs (use head SHA from step 1)
gh api repos/{org}/{repo}/statuses/{head_sha} \
  --jq '.[] | select(.state == "failure" or .state == "error") | {context, state, target_url}'
```

This gives you the list of failed jobs and their Prow dashboard URLs.

### 3. Construct GCS artifact URLs

From a Prow `target_url` like:
```
https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/{org}_{repo}/{pr}/{job_name}/{build_id}
```

Derive:
- **Directory browser** (for navigating artifact tree):
  `https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results/pr-logs/pull/{org}_{repo}/{pr}/{job_name}/{build_id}/`
- **Raw file content** (for fetching logs and JSON):
  `https://storage.googleapis.com/test-platform-results/pr-logs/pull/{org}_{repo}/{pr}/{job_name}/{build_id}/{path}`

### 4. Triage the failure

For each failed job, fetch artifacts in this order:

#### 4a. Quick status

```
GET storage.googleapis.com/.../finished.json
```

Check `"passed": false` and `"result": "FAILURE"`.

#### 4b. Build log (most useful)

```
GET storage.googleapis.com/.../build-log.txt
```

This is the main ci-operator build log. It can be large (200KB+). Search from the **end** for:
- `failed` / `FAILED` / `error` / `ERROR`
- `step .* failed`
- Test runner failures (`FAIL`, `--- FAIL`, `Traceback`, `AssertionError`, `Error:`)
- Container crash indicators (`CrashLoopBackOff`, `OOMKilled`, `Error from server`)

#### 4c. Artifact tree exploration

The build log alone often doesn't tell the full story. Browse the GCS artifact directory for step-specific logs, JUnit, and cluster state:

```
GET gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/.../artifacts/
```

Typical layout (names vary by job and workflow):

```
{build_id}/
├── build-log.txt                 ← main ci-operator log (start here)
├── finished.json                 ← pass/fail + metadata
└── artifacts/
    ├── ci-operator.log
    ├── junit_operator.xml        ← top-level JUnit when present
    ├── ci-operator-step-graph.json  ← step names and order (use to find subdirs)
    ├── build-logs/               ← image build logs (when images are built)
    ├── build-resources/          ← CI namespace snapshots
    │   ├── pods.json
    │   └── events.json
    └── <step-name>/              ← one directory per workflow step
        ├── build-log.txt
        ├── finished.json
        └── artifacts/            ← step-specific (junit XML, gathered cluster YAML, pod logs)
```

**Where to look by failure type:**

| Symptom | Check these artifacts |
|---------|------------------------|
| Unit test / lint failure | Root `build-log.txt`; step dirs whose names match `test`, `verify`, or `unit` |
| JUnit / pytest / go test output | `junit*.xml` under `artifacts/` or step `artifacts/` |
| Image build failure | `build-logs/*.log` |
| E2E / cluster test | Deepest `e2e` or `test` step: `build-log.txt` and that step’s `artifacts/` |
| Deployment / pod issues | Step `artifacts/` for `pods.yaml`, `events`, or `podlogs/` if gathered |
| Cluster infra | `gather-*` or must-gather style steps, `events.json` |

Use `ci-operator-step-graph.json` to see which step failed and match it to sibling directories under `artifacts/`.

#### 4d. Downloading artifacts locally

When you need to search across many files or the artifacts are too large for WebFetch, download them to a temp directory using `gsutil` or `gcloud storage`:

```bash
TMPDIR=$(mktemp -d)
gcloud storage cp -r \
  gs://test-platform-results/pr-logs/pull/{org}_{repo}/{pr}/{job_name}/{build_id}/artifacts/<subpath>/ \
  "$TMPDIR/"
```

The GCS bucket path mirrors the Prow URL: strip `https://prow.ci.openshift.org/view/gs/` and prepend `gs://`.

When multiple jobs have failed, investigate each in a separate subagent (Task tool) to keep build-log context isolated and run fetches in parallel.

### 5. Cross-reference with PR changes

Compare the failure with the files changed in the PR. Common patterns:

| Failure type | Likely cause |
|--------------|--------------|
| Unit/integration test failure | Direct code bug in changed files |
| E2E cluster test failure | Infrastructure issue OR deployment-breaking change |
| Verify/lint failure | Formatting, type errors, or import issues |
| Image build failure | Dependency or Dockerfile issue |
| Flaky (passes on retest) | Known flake, not PR-related |

Check if the same job fails on `main` branch (flaky test) by looking at job history:
```
https://prow.ci.openshift.org/job-history/gs/test-platform-results/pr-logs/directory/{job_name}
```

### 6. Report findings

Summarize:
1. **Which jobs failed** and which passed
2. **Root cause** for each failure (with relevant log excerpts)
3. **Whether it's PR-related or infrastructure/flaky**
4. **Suggested fix** if the failure is caused by the PR changes

## Known CI jobs for this repo

Exact Prow contexts and commands are defined in **openshift/release** (ci-operator) for `openshift/{repo}`. Do not assume they match another repository (e.g. Python `make verify` vs Go `go test`).

1. Run `gh pr checks <pr> --repo <org>/<repo>` to list **actual** failing contexts.
2. For this Go operator codebase, failures often cluster as: **unit tests** (`go test`), **lint/verify**, **image build**, or **optional e2e** — confirm from the job’s `build-log.txt` and step names.

`tide` is merge gating (labels/approvals), not a test. **Konflux** (if present) is a separate supply-chain pipeline from classic Prow.

## Tool usage notes

- Use `gh` CLI for all GitHub API calls (PR metadata, statuses, checks, comments, files).
- Use `WebFetch` to browse GCS directories (`gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/...`).
- Use `WebFetch` to fetch raw log/JSON content (`storage.googleapis.com/test-platform-results/...`).
- The Prow dashboard URL itself is JS-rendered and not useful via WebFetch — always use GCS URLs instead.
- Build logs can be very large. When fetched via WebFetch, they're saved to a temp file — read from the end to find failures quickly.
