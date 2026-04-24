# Calibration fixtures

Hand-synthesized request bodies used by the estimator calibration
tests in `summarize_calibration_test.go`. Generated from lorem-
ipsum filler with realistic Claude Code shape (system, 5 tool
schemas, alternating user/assistant turns with thinking blocks,
a 30 KB tool_result, one 1×1 PNG image). No production data, no
real paths, no internal hostnames. Safe to ship.

- `calibration_request.json` — system sent as a string (most
  common Claude Code shape).
- `calibration_request_system_array.json` — same body with
  system as a `[{type:"text", text:...}]` block array
  (Anthropic's other accepted form); used to assert the
  estimator counts both forms consistently.
