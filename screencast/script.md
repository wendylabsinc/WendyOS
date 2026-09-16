# Swift E2E tests: executable specifications for AI-driven development

Audience: Wendy engineers exploring AI-driven development practices.
Goal: Introduce the hypothesis behind executable E2E specifications before
showing the Wendy implementation and workflow.
Tone: Calm, thoughtful, direct.

---

## 01 Title

### Say

Swift E2E tests: executable specifications for AI-driven development.

AI is increasingly doing the implementation work—the how. Human engineers
still decide what the product should do and which outcomes are correct.

That means humans and AI need a shared source of truth focused on those
outcomes.

### Show (slide)

```text
Swift E2E Tests

Executable specifications
for AI-driven development

Humans decide what.
AI implements how.
```

---

## 02 Executable specifications

### Say

As AI capabilities improve, implementation becomes more of a derived artifact:
mostly a function of the specification, closer to compilation than the primary
place where humans collaborate.

The question is what form that specification should take. There is no settled
answer.

One promising idea is to express it as E2E tests. Unlike prose alone, an
executable specification can deterministically validate the intended outcome
against the real product, across different devices and contexts.

The same artifact becomes behavioral documentation, acceptance criteria, and
validation.

### Show (slide)

```text
Shared source of truth

Human-readable outcome
          +
Deterministic execution
          +
Validation against the real product

One promising approach:
specification = executable E2E tests
```

---

## 03 Automate execution, apply AI to judgment

### Say

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

### Show (slide)

```text
Humans
  define outcomes
        ↓
Executable E2E specifications
        ↓
Deterministic automation
  runs across devices and contexts
        ↓
Structured evidence
        ↓
AI implements, analyzes, and iterates
```

---

## 04 To be continued

### Say

That is the premise.

Next, we will look at how Wendy’s Swift E2E system turns it into a practical
development loop—from a human-readable specification, through deterministic
execution, to the evidence that drives the next AI iteration.

To be continued.

### Show (slide)

```text
To be continued…

Next:
specification → execution → evidence → iteration
```
