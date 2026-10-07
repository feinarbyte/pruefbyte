# pruefbyte

AI code review for GitLab merge requests, in the spirit of
[pr-agent](https://github.com/The-PR-Agent/pr-agent). The review itself is done by
[OpenCodeReview](https://github.com/alibaba/open-code-review) (`ocr`). pruefbyte
runs it in a merge request pipeline and posts the findings as inline discussions
from a dedicated bot account.

- Findings are anchored to the right diff line, with GitLab "apply suggestion" blocks where possible.
- Re-running the pipeline never duplicates a comment.
- The bot resolves its own threads once the flagged code has changed and the finding is gone.
- Findings that can't be placed on the diff, or that go over the comment limit, go into a single summary note, which is updated in place.
- Works with every LLM provider OCR supports (Anthropic, OpenAI, Bedrock, DashScope, OpenRouter, …) and with any OpenAI- or Anthropic-compatible endpoint.

## Setup

1. **Bot user.** Create a GitLab user (e.g. `pruefbyte-bot`) and add it to your group or projects as **Developer**. Create a personal access token for it with scope `api`.
2. **CI/CD variables** (group level, masked). Only secrets go here:
   | Variable | Value |
   |---|---|
   | `PRUEFBYTE_GITLAB_TOKEN` | the bot's PAT |
   | `PRUEFBYTE_LLM_API_KEY` | the LLM API key |

   Provider, model and all review settings go into the repository's `.pruefbyte.yml`
   (see [Configuration](#configuration)), so that `pruefbyte local` reviews with exactly
   the same settings. A `PRUEFBYTE_LLM_MODEL` or similar CI variable would override the
   file in CI only, and local runs would no longer match.
3. **Image.** CI publishes `ghcr.io/feinarbyte/pruefbyte` for linux/amd64 and linux/arm64:
   `latest` from `main`, `X.Y.Z` and `X.Y` from `vX.Y.Z` tags, and `sha-<commit>` for each of these pushes.
   If the package is private, give the GitLab runners pull access (a GitHub token with
   `read:packages` in `DOCKER_AUTH_CONFIG`). To build your own:
   ```sh
   docker buildx build --platform linux/amd64,linux/arm64 \
     -t registry.example.com/tools/pruefbyte:latest --push .
   ```
4. **Pipeline.** Include the template in each project (or in a shared CI config):
   ```yaml
   include:
     - project: tools/pruefbyte
       file: templates/pruefbyte.gitlab-ci.yml
   variables:
     PRUEFBYTE_IMAGE: ghcr.io/feinarbyte/pruefbyte:latest
   ```
   The job runs in merge request pipelines (not in merge train pipelines, and not in pipelines that run in a fork, which lack the CI/CD variables), in the `test` stage: if the project defines `stages:`, keep `test` or override the job's `stage:`. If the project's other jobs run in branch pipelines, switch to merge request pipelines while a merge request is open, as GitLab recommends. Otherwise every push runs two pipelines, and the merge check only looks at the merge request pipeline, which then holds nothing but this job:
   ```yaml
   workflow:
     rules:
       - if: $CI_PIPELINE_SOURCE == "merge_request_event"
       - if: $CI_COMMIT_BRANCH && $CI_OPEN_MERGE_REQUESTS
         when: never
       - if: $CI_COMMIT_BRANCH
   ```

## Configuration

Settings are layered; later layers win:

1. Built-in defaults.
2. The global file, from `--config` or `PRUEFBYTE_CONFIG`. See [`pruefbyte.example.yml`](pruefbyte.example.yml) for every key.
3. `.pruefbyte.yml` in the repository, **read from the merge request's base commit**. A merge request can't change its own review settings: config changes take effect once they are merged. The same holds for OCR's own rule files, `.opencodereview/rule.json` and `ocr.rule_file`: pruefbyte reads them at the base commit and passes one rule file that keeps the merge request's copies from applying.
4. Environment variables `PRUEFBYTE_<SECTION>_<KEY>`, e.g. `PRUEFBYTE_REVIEW_MIN_SEVERITY=medium`. Lists are comma-separated.

The repository file may set `llm.provider` (OCR built-in providers only, and only when the global config does not set one: a provider the operator names keeps the shared API key with that vendor), `llm.model`, `ocr.*` (except `binary` and `extra_args`) and `review.*`. Anything that decides where credentials are sent, or what gets executed, is rejected there: custom providers with their own `llm.url` belong in the global file.

`llm.model` can be any model ID the provider serves. With the OCR that pruefbyte bundles (the Docker image and release binaries, OCR v1.12.12 or newer), the model lists of OCR's built-in providers are only suggestions: a model that is not listed works, and OCR logs `[ocr] WARNING: model "…" is not in the suggested models for provider "…"; the provider will validate it`. A wrong ID therefore fails at the provider's API, not earlier. OCR v1.12.10 and older rejects unlisted models; that is what pruefbyte 0.1.0 bundles, and what an older `ocr` on your PATH (with `go install` builds, or set via `ocr.binary`) may still do. `pruefbyte version` shows which OCR is used.

Secrets are only ever read from the env vars named by `gitlab.token_env` and `llm.api_key_env`. `ocr` runs with a private, temporary `HOME`, so its config file and session logs never touch the runner.

Example `.pruefbyte.yml`:

```yaml
llm:
  provider: anthropic
  model: claude-sonnet-5
ocr:
  effort: high
  exclude: ["**/generated/**"]
  rules:
    - path: "**/*.go"
      rule: "Errors must be wrapped with %w; flag bare returns of err from external calls."
      merge_system_rule: true # add to OCR's built-in Go rules instead of replacing them
review:
  min_severity: medium
  max_comments: 15
```

## When it runs

pruefbyte reviews the merge request whenever it is run; it has no draft, label,
author or branch filters of its own. When it runs is decided by the CI job's
`rules:`. Override the job in your project to add conditions, for example:

```yaml
pruefbyte-review:
  rules:
    - if: $CI_MERGE_REQUEST_EVENT_TYPE == "merge_train"
      when: never
    - if: $CI_PROJECT_ID != $CI_MERGE_REQUEST_PROJECT_ID
      when: never
    - if: $CI_MERGE_REQUEST_DRAFT == "true"             # skip drafts
      when: never
    - if: $CI_MERGE_REQUEST_LABELS =~ /(^|,)no-review(,|$)/
      when: never
    - if: $GITLAB_USER_LOGIN == "renovate-bot"
      when: never
    - if: $CI_MERGE_REQUEST_TARGET_BRANCH_NAME =~ /^release\//
      when: never
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
```

Overriding `rules:` replaces the template's list, so keep its first two entries
(merge trains, fork pipelines). To review on demand only, use `when: manual`.

## Behaviour

| Situation | What happens |
|---|---|
| MR has new commits since the pipeline started | Skipped; the newer pipeline reviews it. |
| Finding on a line in the diff | Inline discussion. Suggestion block if OCR proposed replacement code for exactly the lines it quotes. |
| Finding about removed code | Inline discussion on the removed line, without a suggestion block. |
| Finding outside the diff, or GitLab rejects the position | Listed in the summary note. |
| Same finding as an earlier run (open or resolved) | Not posted again. Matched by a hidden fingerprint of file + the code the finding quotes (its text if it quotes none), so rewording doesn't count as new. |
| Earlier bot thread not reported again **and** its code changed | Reply and resolve, but only after a complete review that covered the file, and only once: a thread someone reopens stays open. |
| OCR fails | A failure note with the redacted error; job exits 1. |
| `review.fail_on_severity` reached | Comments are posted; job exits 3. |

## Local review

Run the CI review on your machine before you push, so the bot has nothing left to say:

```sh
pruefbyte local
```

It reviews what your merge request will contain: everything from the merge base with
the target branch (default: origin's HEAD; or e.g. `--target origin/develop`) up to your
working tree, including staged, unstaged and untracked files. Your index, branch and
files stay untouched. `--committed` reviews only commits, which is exactly what CI sees
after a push. Findings print in the terminal (`--format json` for tools), and nothing
is posted.

The settings are the CI's, read the same way and from the same places: `.pruefbyte.yml`
and OCR rule files at the target branch, with the same rule merging, excludes, effort,
provider and model, and the same `review.min_severity` / `review.categories` filtering.
A finding that reaches `review.fail_on_severity` exits 3, as in CI, so the command
works as a pre-push hook. If you edit `.pruefbyte.yml` on your branch, the run tells
you that CI, and so the local run, uses the target branch's version until your change
is merged. One difference remains: CI gives OCR the merge request's title and
description as background; locally the branch name and commit messages stand in.

The API key is the first one found of:
1. the env var named by `llm.api_key_env` (`PRUEFBYTE_LLM_API_KEY`),
2. your own OCR setup (`~/.opencodereview/config.json`: `api_key` or `api_key_cmd`),
3. the provider's env var, e.g. `ANTHROPIC_API_KEY`.

No GitLab token is needed. If your CI uses a global config file (`PRUEFBYTE_CONFIG`),
pass the same file with `--config`.

### Install

Release binaries include OpenCodeReview (`ocr`), the exact version CI uses, so a
single download is all you need. On first use pruefbyte unpacks it into your user cache
directory. Pick whichever install suits you:

**With mise:**

```sh
mise use -g github:feinarbyte/pruefbyte
```

**Linux / macOS**, latest release into `~/.local/bin`:

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/feinarbyte/pruefbyte/releases/latest/download/pruefbyte_${os}_${arch}.tar.gz" \
  | tar -xz -C ~/.local/bin pruefbyte
```

**Windows** (PowerShell), latest release into `%LOCALAPPDATA%\Programs\pruefbyte`:

```powershell
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir = "$env:LOCALAPPDATA\Programs\pruefbyte"; $zip = "$env:TEMP\pruefbyte.zip"
Invoke-WebRequest "https://github.com/feinarbyte/pruefbyte/releases/latest/download/pruefbyte_windows_$arch.zip" -OutFile $zip
Expand-Archive $zip $dir -Force; Remove-Item $zip
[Environment]::SetEnvironmentVariable('Path', "$([Environment]::GetEnvironmentVariable('Path', 'User'));$dir", 'User')
```

Each release also lists the archives with a `checksums.txt`
([releases](https://github.com/feinarbyte/pruefbyte/releases)).

**With Go** 1.25 or newer. This builds from source without OCR, so `ocr` must be on
your PATH as well:

```sh
go install github.com/feinarbyte/pruefbyte/cmd/pruefbyte@latest
npm install -g @alibaba-group/open-code-review   # or: brew install open-code-review
```

**With Docker**, with `ocr` included and nothing to install. Run it as yourself so
new git objects in your repository stay yours:

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/repo" -w /repo \
  -e PRUEFBYTE_LLM_API_KEY ghcr.io/feinarbyte/pruefbyte pruefbyte local
```

Check the install with `pruefbyte version`; it also says whether `ocr` is built in.
An `ocr.binary` setting overrides the built-in copy.

To try the CI path against a real merge request without posting, set
`PRUEFBYTE_GITLAB_TOKEN` and run `pruefbyte review --project group/project --mr 42 --dry-run`.
`pruefbyte config print` shows the effective configuration.

## Development

```sh
mise install            # Go toolchain and golangci-lint
gofmt -l .              # must print nothing
golangci-lint run ./...
go test -race ./...
```

GitHub Actions (`.github/workflows/ci.yml`) runs these checks plus `go mod tidy` on
pushes to `main` and `v*` tags and on every pull request. It also builds all release
binaries (Linux, macOS and Windows; amd64 and arm64) with GoReleaser
(`.goreleaser.yaml`). Once these pass, it builds the multi-arch image. On `main` and
`v*` tags the image is pushed to GHCR; for pull requests it is only built.

To release, push a tag such as `v0.1.0`. CI then publishes the image tags `0.1.0`
and `0.1`, plus a GitHub release with the binaries and checksums. Try the release
build locally with `goreleaser release --snapshot --clean`.

The OCR version is pinned in `internal/ocrbin/VERSION`, for both the Docker image and
the release binaries. To update OCR, change that file. Release builds use the
`embedocr` build tag and embed the gzip-compressed `ocr` that
`go run ./internal/ocrbin/fetch` downloads and checks against OCR's published
checksums. Plain `go build` and `go test` need neither.

Layout:

| Path | Purpose |
|---|---|
| `cmd/pruefbyte` | CLI (cobra) and wiring |
| `internal/config` | layered config, repo-file allow-list, env overrides |
| `internal/ocr` | drives the `ocr` CLI and parses its JSON |
| `internal/ocrbin` | the pinned OCR version; the `ocr` embedded in release builds (`fetch` downloads it) |
| `internal/gitlab` | client-go wrapper bound to one MR; dry-run decorator |
| `internal/review` | orchestration: filter, place, dedupe, publish, resolve |
| `internal/gitutil` | reads the repo config at the base commit; fetches missing commits |

OCR is used as a subprocess. Its Go packages all live under `internal/`, so they can't be imported from another module; its JSON output is the stable interface.

## License

pruefbyte is released under the [MIT License](LICENSE).

The Docker image and the release binaries also bundle the OpenCodeReview (`ocr`)
binary, which is licensed under the [Apache License 2.0](https://github.com/alibaba/open-code-review/blob/main/LICENSE).
Both license texts are in the image under `/usr/share/licenses/`, and in each release
archive as `LICENSE` and `LICENSE.open-code-review`.
