# Repository Guidelines

This repository is organized as a monorepo containing multiple applications and shared components.

## Repository layout

```text
src/
  api/            # Go service, CLI, migrations, and package tests
  dashboard/      # React dashboard
sdks/
  react-native/
  swift/
  android/
  compose/        # Jetpack Compose helpers
tests/
  contracts/      # Shared API contract fixtures across languages
  e2e/            # Tests spanning the API and SDKs
examples/
docs/
deploy/
```

- Keep applications under `src/` and independently published SDKs under root-level `sdks/`. Each application or SDK owns its dependencies and build configuration.
- Keep unit and integration tests with the application or SDK they exercise. Go tests, including database integration tests, stay alongside their packages in `src/api`.
- Reserve root-level `tests/` for shared contract fixtures and end-to-end tests spanning applications or SDKs.
- Add planned directories when implementing their contents; do not create empty placeholders.
- Keep the detailed layout in `PROJECT.md` and the overview in `README.md` consistent with these rules.

## Architecture

Organize application code by vertical slice. Group files by business capability and action rather than by technical layer.

A typical slice follows this shape:

```text
<Area>/<Feature>/.../<Action>/
```

Keep the files needed for an action close together. Depending on the application, a slice may contain:

- Input and output contracts
- Business logic
- Entry points or request handlers
- User interface and presentation logic
- State management
- Tests

Place domain concepts and shared feature-level infrastructure at the nearest common feature directory. Move code into a global shared area only when it is genuinely reused across unrelated slices.

## Working Conventions

- Follow the existing naming and structure in the area being changed.
- Keep changes focused on the requested capability.
- Prefer extending an existing slice over introducing a parallel structure.
- Keep application-specific details within that application's directory.
- Avoid coupling independent applications through implementation details.
- Update relevant tests and documentation when behavior changes.
- Check for more specific `AGENTS.md` instructions in the directory being modified; deeper instructions take precedence over this file.
