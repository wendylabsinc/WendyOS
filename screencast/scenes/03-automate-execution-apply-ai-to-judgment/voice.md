AI-driven development does not mean asking AI to perform every validation step
ad hoc.

That would be less reproducible and unnecessarily expensive in token usage.
Repeatable mechanical work should be scripted once and executed cheaply as
often as needed.

The test runner handles deterministic execution and records structured
evidence. AI works at the higher-value layer: implementing the specification,
analyzing failures, and iterating on the result.

Humans define the outcomes. Automation executes them. AI implements and
interprets the feedback.

That is the strategy behind Wendy’s Swift E2E system.
