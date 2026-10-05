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
2. **CI/CD variables** (group level, masked):
   | Variable | Value |
   |---|---|
   | `PRUEFBYTE_GITLAB_TOKEN` | the bot's PAT |
   | `PRUEFBYTE_LLM_API_KEY` | the LLM API key |
   | `PRUEFBYTE_LLM_PROVIDER` | e.g. `anthropic` |
   | `PRUEFBYTE_LLM_MODEL` | e.g. `claude-sonnet-5` |
3. **Image.** Build and push it:
   ```sh
   docker build -t registry.example.com/tools/pruefbyte:latest --build-arg OCR_VERSION=v1.12.10 .
   docker push registry.example.com/tools/pruefbyte:latest
   ```
4. **Pipeline.** Include the template in each project (or in a shared CI config):
   ```yaml
   include:
     - project: tools/pruefbyte
       file: templates/pruefbyte.gitlab-ci.yml
   variables:
     PRUEFBYTE_IMAGE: registry.example.com/tools/pruefbyte:latest
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

The repository file may only set `llm.model`, `ocr.*` (except `binary` and `extra_args`), and `review.*`. Anything that decides where credentials are sent, or what gets executed, is rejected there.

Secrets are only ever read from the env vars named by `gitlab.token_env` and `llm.api_key_env`. `ocr` runs with a private, temporary `HOME`, so its config file and session logs never touch the runner.

Example `.pruefbyte.yml`:

```yaml
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

## Local use

```sh
export PRUEFBYTE_GITLAB_TOKEN=glpat-... PRUEFBYTE_LLM_API_KEY=sk-...
pruefbyte review --config pruefbyte.yml --gitlab-url https://gitlab.example.com \
  --project group/project --mr 42 --repo . --dry-run
```

`--dry-run` prints the discussions instead of posting them. `pruefbyte config print` shows the effective configuration.

## Development

```sh
mise install        # Go toolchain
go test ./...
```

Layout:

| Path | Purpose |
|---|---|
| `cmd/pruefbyte` | CLI (cobra) and wiring |
| `internal/config` | layered config, repo-file allow-list, env overrides |
| `internal/ocr` | drives the `ocr` CLI and parses its JSON |
| `internal/gitlab` | client-go wrapper bound to one MR; dry-run decorator |
| `internal/review` | orchestration: filter, place, dedupe, publish, resolve |
| `internal/gitutil` | reads the repo config at the base commit; fetches missing commits |

OCR is used as a subprocess. Its Go packages all live under `internal/`, so they can't be imported from another module; its JSON output is the stable interface.
