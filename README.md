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

## Configuration

Settings are layered; later layers win:

1. Built-in defaults.
2. The global file, from `--config` or `PRUEFBYTE_CONFIG`. See [`pruefbyte.example.yml`](pruefbyte.example.yml) for every key.
3. `.pruefbyte.yml` in the repository, **read from the merge request's base commit**. A merge request can't change its own review settings: config changes take effect once they are merged.
4. Environment variables `PRUEFBYTE_<SECTION>_<KEY>`, e.g. `PRUEFBYTE_REVIEW_MIN_SEVERITY=medium`. Lists are comma-separated.

The repository file may only set `llm.model`, `ocr.*` (except `binary` and `extra_args`), `review.*` and `skip.*`. Anything that decides where credentials are sent, or what gets executed, is rejected there.

Secrets are only ever read from the env vars named by `gitlab.token_env` and `llm.api_key_env`. `ocr` runs with a private, temporary `HOME`, so its config file and session logs never touch the runner.

Example `.pruefbyte.yml`:

```yaml
ocr:
  effort: high
  exclude: ["**/generated/**"]
  rules:
    - path: "**/*.go"
      rule: "Errors must be wrapped with %w; flag bare returns of err from external calls."
review:
  min_severity: medium
  max_comments: 15
skip:
  authors: [renovate-bot]
```

## Behaviour

| Situation | What happens |
|---|---|
| Draft MR, skip label/author/branch/title | Job exits 0 without reviewing. |
| MR has new commits since the pipeline started | Skipped; the newer pipeline reviews it. |
| Finding on a line in the diff | Inline discussion. Suggestion block if OCR proposed replacement code. |
| Finding outside the diff, or GitLab rejects the position | Listed in the summary note. |
| Same finding as an earlier run (open or resolved) | Not posted again. Matched by a hidden fingerprint of file + text. |
| Earlier bot thread not reported again **and** its code changed | Reply and resolve, but only after a complete review. |
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
