# Contributing to protolsp

Thanks for your interest in contributing! This repository is the Go
language server for the [PXF text format](https://github.com/trendvidia/protowire):
schema-aware diagnostics, hover, completion, and navigation for `.pxf`
files in any LSP-capable editor. It is part of the
[protowire](https://github.com/trendvidia/protowire) family of projects.

## Quick Start

```bash
git clone https://github.com/trendvidia/protolsp.git
cd protolsp
go test ./...        # full test suite
go vet ./...         # static checks
```

The module depends on a small fork of `google.golang.org/protobuf` that
adds the namespace-aware registry types
[protoregistry](https://github.com/trendvidia/protoregistry) needs. The
fork is pulled in via a `replace` directive in `go.mod` and points at
the public release at
[`github.com/trendvidia/protobuf-go`](https://github.com/trendvidia/protobuf-go).
You do not need a local checkout.

The default build is the community build. Building with
`-tags protocheck` adds protocheck runtime validation and needs modules
that are not yet public; CI runs it for branches of this
repository.

## What Makes a Good PR

- **One concern per PR.** Bug fix, feature, or refactor — pick one.
- **Tests first, or tests with the change.** A change to what an editor
  sees (hover, diagnostics, completion, code actions) gets a test in
  `internal/server` that drives the server over the LSP surface, not
  only a unit test of the helper.
- **Format and schema changes go through the spec repo.** PXF grammar
  and annotation semantics are defined in
  [`trendvidia/protowire`](https://github.com/trendvidia/protowire) so
  every implementation stays compatible. Open a spec PR there *first*,
  then implement the change here.
- **Custom LSP methods and the Connect-RPC services are a contract.**
  Editors and embedders depend on their shapes; say in the PR body when
  you change one, and record it in [`CHANGELOG.md`](CHANGELOG.md).

## Code Style

- Run `gofmt` before committing — CI will reject otherwise.
- Public API documentation goes on every exported symbol, in full
  sentences, leading with the symbol name.
- Prefer returning `error` over `panic`. A malformed document is normal
  input for a language server, never a reason to crash it.
- Avoid new dependencies. A PR that adds a third-party module will be
  asked to justify it.

## When CI Fails

Read the log before rerunning anything, and classify first:

- **An infrastructure drop** — a job with zero step results, a lost
  runner, a cache fetch that timed out. Rerun it, and say that is why.
- **A real regression** — the failure reproduces locally at `-count=1`.
  Fix it.
- **A flake** — the failure does not reproduce on every run. A flake is
  a real bug at low frequency (a shutdown race, an ordering assumption,
  a byte-for-byte comparison of something whose order is not promised),
  and it gets an issue that states the **measured rate**: run the test
  at `-count=100` or more and read the counts. It does not get a retry
  annotation — a retry hides the rate, and the rate is the diagnosis.

CI runs one job on GitHub-hosted runners with stock Go: the untagged
build, which needs no private module, for every pull request, forks
included. The `-tags protocheck` build links the private govm and
protocheck-govm modules, so CI does not run it; if you have access to
them, run `go test -tags protocheck ./...` before you push.

## Reporting Bugs

Open a GitHub issue with a reproducer (a minimal workspace, an LSP
message trace, or both). Please state the commit / tag you observed the
bug on. For security-sensitive reports, see [`SECURITY.md`](SECURITY.md)
instead — those should never go through public issues.

## Code of Conduct

Participation is governed by [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

---

# Governance Addendum: Steward

This repository is managed by **Steward** — an autonomous AI governance
agent that reviews, scores, and merges pull requests. There are no
human gatekeepers in the merge path. Code is evaluated, scored, and
merged based on objective, deterministic rules. The public surface of
Steward lives at <https://steward-dev.ai>.

## 1. How Code Gets Merged (the Lifecycle)

1. **You open a Pull Request.**
2. **Steward evaluates it.** Steward runs static analysis, evaluates
   cyclomatic complexity, checks dependency licenses, and calculates
   the required reputation threshold based on the files you touched.
3. **Steward outputs a Diagnostic Log.** Within seconds, Steward
   comments on your PR with a detailed breakdown of its mathematical
   evaluation.
4. **Community Voting (if required).** If your code passes all security
   and quality gates, Steward opens the PR for weighted voting.
5. **Auto-Merge.** If the required consensus threshold is met, Steward
   merges the code automatically.

## 2. The Escrow Pipeline (For New Contributors)

If this is your first time contributing, you currently have a
Reputation Vector of `0`.

**Do not submit massive, architectural rewrites as your first PR.** If
you submit a PR that modifies core infrastructure and you have `0`
reputation, Steward will automatically place your PR into **Escrow
Status**.

**How to unlock Escrow:**

1. Steward will automatically generate and assign you 2–3 isolated
   issues labeled `sandbox`.
2. These are safe, low-blast-radius tasks (improving test coverage,
   documentation, etc.).
3. Once you successfully merge the `sandbox` issues, your reputation
   vector increases, and Steward will automatically unlock your
   original PR for the community vote.

## 3. Private Mentorship Mode

If you want Steward to evaluate your code *without* posting a public
review comment on the PR timeline, use **Draft Mode**.

- Open your Pull Request as a **Draft**.
- Steward will evaluate your code and send its feedback as a private
  review visible only to you.
- Use this mode to iterate on performance bottlenecks or style
  violations privately. Once the math looks good, mark the PR as
  **Ready for Review** to trigger the public evaluation.

## 4. How to Read a Rejection Log

Steward does not reject code based on opinions. If your PR is blocked,
the diagnostic log will state the mathematical or constitutional
reason. Example:

> **Action: Blocked**
> - **Reason:** Cyclomatic complexity threshold exceeded.
> - **Details:** `analysis/hover.go` introduced a nested loop with
>   a complexity score of 12. The Constitution limits this domain to a
>   maximum score of 8.
> - **Resolution:** Refactor the parsing logic to flatten the condition
>   tree before requesting a re-evaluation.

Do not argue with Steward in the comments. Refactor the code to
satisfy the metric, push the commit, and Steward will automatically
re-evaluate.

## 5. Useful Commands

You can interact with Steward by commenting on issues or PRs:

- `/steward evaluate` — Forces Steward to immediately re-run the
  9-dimension check on your latest commit.
- `/steward check-reputation` — Steward replies with your current
  decayed reputation vectors and your effective voting weight for the
  current PR's domain.
- `/steward sandbox-me` — Type this on any open issue, and Steward
  will find an unassigned, low-risk issue and assign it to you to
  help build your initial reputation.

## 6. Reporting Bugs Through Steward

If Steward verifies that a bug was introduced in a previously merged
PR, it will retroactively slash the original author's reputation
vector. Please write thorough unit tests.
