# AGENTS.md

Guidance for coding agents working in this repository.

## Commits

Write commit messages in the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) format:

```
<type>(<optional scope>): <description>

<optional body>

<optional footers>
```

- **Type:** one of `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore` or `revert`.
- **Scope:** optional, and names the area changed, such as `review`, `local`, `ocr`, `config`, `gitlab` or `gitutil`.
- **Description:** imperative, lower case, no trailing period, and short enough to fit on one line.
- **Body:** explains what changed and why, separated from the subject by a blank line.
- **Breaking changes:** mark them with `!` after the type or scope, and add a `BREAKING CHANGE:` footer that describes the change.

Examples:

```
feat(review): reuse reviews of files whose diff did not change
fix(local): clear the stored state after a failed --full run
docs: document the pruefbyte-full label
```
