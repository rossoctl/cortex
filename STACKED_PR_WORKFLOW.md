# Stacked PR Workflow — kagenti-extensions

This document describes how to work with stacked PRs in this repository, including the critical rebase process when earlier branches receive changes.

## Current Stack: Vault Pattern Implementation

```
main
 │
 ├─> PR #379: feat/vault-integration (Phase 1)
 │    └─ authlib/vault library
 │    └─ Branch: feat/vault-integration
 │
 └──> PR #380: feat/vault-fetcher-cli (Phase 2)
      │ └─ vault-fetcher CLI tool
      │ └─ Branch: feat/vault-fetcher-cli
      │ └─ Base: feat/vault-integration
      │
      └──> PR #TBD: feat/vault-webhook (Phase 3)
           │ └─ Webhook integration docs
           │ └─ Branch: feat/vault-webhook
           │ └─ Base: feat/vault-fetcher-cli
           │
           └──> PR #TBD: feat/vault-docs-polish (Phase 4)
                └─ CI/CD, docs, polish
                └─ Branch: feat/vault-docs-polish
                └─ Base: feat/vault-webhook
```

## Understanding Stacked PRs

### What is a Stacked PR?

A stacked PR is when one pull request's base branch is another feature branch (not `main`).

**Example:**
- PR #379: `feat/vault-integration` → `main`
- PR #380: `feat/vault-fetcher-cli` → `feat/vault-integration` ← Stacked!

### Benefits

1. **Parallel development** — Don't wait for Phase 1 approval to start Phase 2
2. **Isolated reviews** — Each PR shows only its changes
3. **Clear dependencies** — Stack structure shows what depends on what
4. **Faster iteration** — Continue working while earlier PRs are in review

### How GitHub Shows Stacked PRs

When viewing PR #380 (Phase 2):
- **Files changed:** Only shows vault-fetcher files (not authlib files)
- **Commits:** Shows Phase 2 commits + all Phase 1 commits
- **Base branch:** `feat/vault-integration` (not `main`)

## Creating Stacked PRs

### Method 1: Manual (What We Used)

```bash
# Create Phase 1
git checkout -b feat/vault-integration main
# ... make changes ...
git commit -m "feat: Phase 1"
git push -u origin feat/vault-integration
gh pr create --base main --title "Phase 1" --draft

# Create Phase 2 (stacked on Phase 1)
git checkout -b feat/vault-fetcher-cli feat/vault-integration
# ... make changes ...
git commit -m "feat: Phase 2"
git push -u origin feat/vault-fetcher-cli
gh pr create --base feat/vault-integration --title "Phase 2" --draft

# Create Phase 3 (stacked on Phase 2)
git checkout -b feat/vault-webhook feat/vault-fetcher-cli
# ... make changes ...
git commit -m "feat: Phase 3"
git push -u origin feat/vault-webhook
gh pr create --base feat/vault-fetcher-cli --title "Phase 3" --draft

# And so on...
```

### Method 2: gh-stack (Requires Repo Settings)

```bash
# Initialize stack
gh stack init --adopt feat/vault-integration feat/vault-fetcher-cli feat/vault-webhook feat/vault-docs-polish

# View stack
gh stack view

# Submit all PRs
gh stack submit --draft
```

**Note:** `gh stack submit` requires "Stacked PRs" to be enabled in repository settings.

## The Critical Rebase Process

### When Earlier Branches Get Changes

**Scenario:** PR #379 (Phase 1) receives review feedback. You make changes to `feat/vault-integration`.

**Problem:** Branches 2, 3, and 4 are now out of sync!

```
main
 │
 ├─> feat/vault-integration (UPDATED ✏️)
      │
      └──> feat/vault-fetcher-cli (OUT OF SYNC ⚠️)
           │
           └──> feat/vault-webhook (OUT OF SYNC ⚠️)
                │
                └──> feat/vault-docs-polish (OUT OF SYNC ⚠️)
```

### Solution: Cascade Rebase

**You MUST rebase all subsequent branches onto the updated branch.**

```bash
# Step 1: Make changes to Phase 1
git checkout feat/vault-integration
# ... make changes based on review feedback ...
git commit -m "fix: address review feedback"
git push --force-with-lease origin feat/vault-integration

# Step 2: Rebase Phase 2 onto updated Phase 1
git checkout feat/vault-fetcher-cli
git rebase feat/vault-integration
# Resolve conflicts if any
git push --force-with-lease origin feat/vault-fetcher-cli

# Step 3: Rebase Phase 3 onto updated Phase 2
git checkout feat/vault-webhook
git rebase feat/vault-fetcher-cli
# Resolve conflicts if any
git push --force-with-lease origin feat/vault-webhook

# Step 4: Rebase Phase 4 onto updated Phase 3
git checkout feat/vault-docs-polish
git rebase feat/vault-webhook
# Resolve conflicts if any
git push --force-with-lease origin feat/vault-docs-polish
```

### Rebase Checklist

After making changes to ANY branch in the stack:

- [ ] **Identify affected branches** — All branches above the changed one
- [ ] **Rebase in order** — Start from the immediate child, work upward
- [ ] **Test after each rebase** — Ensure builds still work
- [ ] **Push with --force-with-lease** — Safer than --force
- [ ] **Notify reviewers** — Comment on PRs that rebase occurred

### Example: Fixing Phase 1 After Review

```bash
# Reviewer says: "authlib/vault/config.go needs better validation"

# 1. Fix Phase 1
git checkout feat/vault-integration
vim authbridge/authlib/vault/config.go
git add authbridge/authlib/vault/config.go
git commit -m "fix: improve config validation per review"
git push --force-with-lease origin feat/vault-integration

# 2. Cascade rebase
git checkout feat/vault-fetcher-cli
git rebase feat/vault-integration
git push --force-with-lease origin feat/vault-fetcher-cli

git checkout feat/vault-webhook
git rebase feat/vault-fetcher-cli
git push --force-with-lease origin feat/vault-webhook

git checkout feat/vault-docs-polish
git rebase feat/vault-webhook
git push --force-with-lease origin feat/vault-docs-polish

# 3. Comment on PRs
gh pr comment 379 --body "Updated based on review feedback"
gh pr comment 380 --body "Rebased onto updated Phase 1"
gh pr comment 381 --body "Rebased onto updated Phase 2"
gh pr comment 382 --body "Rebased onto updated Phase 3"
```

## Handling Conflicts

### Conflict During Rebase

```bash
git checkout feat/vault-fetcher-cli
git rebase feat/vault-integration

# If conflicts occur:
# CONFLICT (content): Merge conflict in authbridge/go.work
Auto-merging authbridge/go.work
CONFLICT (content): Merge conflict in authbridge/go.work
```

**Resolution:**

```bash
# 1. See what files have conflicts
git status

# 2. Edit conflicted files
vim authbridge/go.work

# 3. Look for conflict markers
<<<<<<<HEAD
(your changes)
=======
(incoming changes)
>>>>>>>

# 4. Resolve conflicts, remove markers
# 5. Stage resolved files
git add authbridge/go.work

# 6. Continue rebase
git rebase --continue

# 7. Push
git push --force-with-lease origin feat/vault-fetcher-cli
```

### Aborting a Rebase

If things go wrong:

```bash
git rebase --abort
# Returns to state before rebase started
```

## Merging Strategy

### Order of Merging

**Always merge from bottom to top (Phase 1 → 2 → 3 → 4).**

```bash
# 1. Merge Phase 1
gh pr merge 379 --squash  # or --rebase, or --merge

# 2. Update Phase 2's base to main
gh pr edit 380 --base main
# Or GitHub may offer to do this automatically

# 3. Merge Phase 2
gh pr merge 380 --squash

# 4. Update Phase 3's base to main
gh pr edit 381 --base main

# 5. Merge Phase 3
gh pr merge 381 --squash

# 6. Update Phase 4's base to main
gh pr edit 382 --base main

# 7. Merge Phase 4
gh pr merge 382 --squash
```

### Alternative: Squash Before Merging

```bash
# After Phase 1 merges, you can squash Phase 2-4 together
git checkout feat/vault-docs-polish  # Top of stack
git rebase -i main

# Interactive rebase: squash commits
# Then PR #382 (Phase 4) contains all changes from Phases 2-4
# Merge just that one PR
```

## Common Scenarios

### Scenario 1: Adding New Branch Mid-Stack

**Want to add Phase 2.5 between Phase 2 and 3:**

```bash
# Create branch from Phase 2
git checkout feat/vault-fetcher-cli
git checkout -b feat/vault-fetcher-tests

# Make changes
git commit -m "feat: add vault-fetcher tests"
git push -u origin feat/vault-fetcher-tests

# Update Phase 3 to stack on new branch
git checkout feat/vault-webhook
git rebase feat/vault-fetcher-tests
git push --force-with-lease origin feat/vault-webhook

# Create PR for new branch
gh pr create --base feat/vault-fetcher-cli --title "Phase 2.5: vault-fetcher tests"
```

### Scenario 2: Removing Branch from Stack

**Want to remove Phase 3, merge Phase 4 directly to Phase 2:**

```bash
# Rebase Phase 4 onto Phase 2
git checkout feat/vault-docs-polish
git rebase feat/vault-fetcher-cli
git push --force-with-lease origin feat/vault-docs-polish

# Update PR base
gh pr edit 382 --base feat/vault-fetcher-cli

# Close Phase 3 PR
gh pr close 381
```

### Scenario 3: Cherry-Picking Fix Across Stack

**Fix in Phase 1 needs to apply to all branches:**

```bash
# Make fix in Phase 1
git checkout feat/vault-integration
git commit -m "fix: critical bug in auth.go"
git push --force-with-lease origin feat/vault-integration

# Rebase all subsequent branches (CASCADE!)
git checkout feat/vault-fetcher-cli && git rebase feat/vault-integration && git push --force-with-lease
git checkout feat/vault-webhook && git rebase feat/vault-fetcher-cli && git push --force-with-lease
git checkout feat/vault-docs-polish && git rebase feat/vault-webhook && git push --force-with-lease
```

## Automation with gh-stack

### Set Up gh-stack

```bash
gh extension install github/gh-stack
gh stack init --adopt feat/vault-integration feat/vault-fetcher-cli feat/vault-webhook feat/vault-docs-polish
```

### Useful Commands

```bash
# View current stack
gh stack view

# Rebase entire stack automatically
gh stack rebase

# Push all branches
gh stack push

# Navigate stack
gh stack up    # Move up one branch
gh stack down  # Move down one branch
gh stack top   # Jump to top of stack
gh stack bottom  # Jump to bottom of stack
```

## Best Practices

1. **Small, focused PRs** — Each phase should be reviewable independently
2. **Rebase immediately** — When changes land in earlier branches
3. **Test after rebase** — Run builds/tests after each rebase
4. **Use --force-with-lease** — Safer than --force
5. **Communicate rebases** — Comment on PRs when rebasing
6. **Keep stack shallow** — 3-5 branches max (we have 4, perfect)
7. **Document dependencies** — PR descriptions should mention stack position

## Troubleshooting

### "Your branch has diverged"

```bash
# After rebasing
error: failed to push some refs
hint: Updates were rejected because the tip of your current branch is behind
```

**Solution:** Use `--force-with-lease` (safe) or `--force` (less safe)

```bash
git push --force-with-lease origin feat/vault-fetcher-cli
```

### "Cannot rebase: You have unstaged changes"

```bash
git status
# Uncommitted changes

# Option 1: Commit them
git add . && git commit -m "wip"

# Option 2: Stash them
git stash
git rebase feat/vault-integration
git stash pop
```

### PR Shows Wrong Diff

**Problem:** PR #380 shows authlib changes (from Phase 1)

**Cause:** Base branch is wrong

**Solution:**
```bash
gh pr edit 380 --base feat/vault-integration
```

## Summary Commands

### Quick Rebase Entire Stack

```bash
# After changing feat/vault-integration
git checkout feat/vault-fetcher-cli && git rebase feat/vault-integration && git push --force-with-lease && \
git checkout feat/vault-webhook && git rebase feat/vault-fetcher-cli && git push --force-with-lease && \
git checkout feat/vault-docs-polish && git rebase feat/vault-webhook && git push --force-with-lease
```

### Check Stack Status

```bash
# Show all PRs in stack
gh pr list --label "vault-pattern"

# Show commit graph
git log --oneline --graph --all feat/vault-integration feat/vault-fetcher-cli feat/vault-webhook feat/vault-docs-polish
```

## References

- [GitHub Stacked PRs Documentation](https://github.blog/changelog/2023-09-27-stacked-pull-requests/)
- [gh-stack Extension](https://github.com/github/gh-stack)
- [Git Rebase Documentation](https://git-scm.com/docs/git-rebase)

---

**Remember:** When in doubt, **cascade rebase**. It's better to rebase too often than not enough!
