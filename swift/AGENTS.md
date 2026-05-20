# Swift agent instructions

## Local macOS agent workflow

When you need to run the local macOS app-backed agent for testing:

1. Start it with:

   ```sh
   make agent-start
   ```

   This will:
   - quit any existing `WendyAgentMac`
   - build the dev app
   - open `Build/WendyAgentMac.app`

2. Wait for the app and agent to finish starting before running CLI tests.

3. Run CLI tests against the local agent from `../go/`, for example:

   ```sh
   cd ../go
   go build -o bin/wendy ./cmd/wendy
   ./bin/wendy <command>
   ```

   If you only need an ad hoc invocation, this is also acceptable:

   ```sh
   cd ../go
   go run ./cmd/wendy <command>
   ```

4. When finished, stop the macOS app with:

   ```sh
   make agent-stop
   ```

## Expectations

- Do not leave `WendyAgentMac` running after tests complete.
- Prefer `make agent-start` and `make agent-stop` over launching or killing the app manually.
- Prefer deterministic smoke-test commands over exploratory manual testing when possible.

## Formatting

- After making changes in `swift/`, always run:

  ```sh
  make format
  ```

- Do this before committing.
