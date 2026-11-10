# Security Policy

4AiCoding runs untrusted code by design: every sandbox executes arbitrary
workloads, and the control plane orchestrates those containers through the
host's Docker daemon. Please report security problems privately.

## Reporting a vulnerability

**Do not open a public issue for security problems.** Use GitHub's private
vulnerability reporting:
[Security → Report a vulnerability](https://github.com/kungfufanta/4AiCoding/security/advisories/new).

Please include:
- the affected component (control plane API / lifecycle / Traefik routing /
  sandbox base image) and the commit you tested;
- reproduction steps and the impact you observed (for example sandbox escape,
  cross-sandbox access, auth bypass, or reaching the control-plane API from a
  sandbox).

## Scope and threat model

Workloads inside a sandbox are treated as hostile. The controls the project
relies on, and the ones still being built, are listed in
[PLAN.md §8 "Security baseline"](PLAN.md). A finding that defeats any control
marked as present there is in scope.
