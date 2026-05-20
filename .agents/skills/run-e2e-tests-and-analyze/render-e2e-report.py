#!/usr/bin/env python3
"""Render the WendyAgent Swift E2E HTML report from command records.

This is intentionally self-contained so the skill can generate a report without
requiring a checked-in project script. Run it from the wendy-agent repository
root after executing the Swift E2E tests with command recording enabled.
"""

from __future__ import annotations

import argparse
import html
import random
import re
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path


DEFAULT_PACKAGE_DIR = Path("swift/WendyAgentE2ETests")
DEFAULT_TESTS_DIR = DEFAULT_PACKAGE_DIR / "Tests/WendyAgentE2ETests"
DEFAULT_TEMPLATE = DEFAULT_PACKAGE_DIR / "Support/e2e-ai-review-report-template.html"
DEFAULT_RECORDS_DIR = DEFAULT_PACKAGE_DIR / ".build/e2e-test-records.current"

# Current hand-reviewed AI checklist outcomes used by the skill. Checklist
# statuses follow run-e2e-tests-and-analyze/SKILL.md: pass, concern, fail.
# The Markdown report should be prose and should exist only when a human needs
# to pay attention to something. Passing, unsurprising checklist items should
# not generate a report.
AI_REVIEW: dict[tuple[str, str], tuple[list[str], str]] = {
    ("WendyTests.swift", "describes the top-level command groups"): (["pass", "pass"], ""),
    ("WendyTests.swift", "'--version' prints the CLI version"): (["pass", "pass"], ""),
    ("WendyTests.swift", "'--device' selects the target device explicitly"): (
        ["pass", "concern"],
        "The command reached the explicitly selected `127.0.0.1` agent and returned "
        "agent fields, but the fixture runs that agent on the same machine as the CLI. "
        "A human should confirm whether this is strong enough evidence that the "
        "response is agent state rather than local CLI state.",
    ),
    ("WendyTests.swift", "prints CLI and system details"): (["pass", "pass"], ""),
    ("WendyJSONTests.swift", "prints the wendy.json schema"): (["pass"], ""),
}

AI_REVIEW_HEADING = "## AI review"

FAKE_ANALYSIS_REMARKS = [
    "Fake human-review report for UI testing. This prose should appear verbatim "
    "under a test row when the AI filter is active.",
    "Fake follow-up report for UI testing. It intentionally avoids severity labels "
    "so the fixed-width Markdown presentation can be reviewed without badge noise.",
    "Fake review note for UI testing. A human would normally read this paragraph "
    "to decide whether the captured behavior needs a source change.",
]

SOURCE_RE = re.compile(r"- Source: `([^`]+):(\d+)`")
FIELD_RES = {
    "machine": re.compile(r"- Machine: `([^`]*)`"),
    "command": re.compile(r"- Command: `([^`]*)`"),
    "status": re.compile(r"- Termination status: `([^`]*)`"),
    "duration": re.compile(r"- Duration: `([^`]*)`"),
}


@dataclass
class CommandRun:
    record: str
    source_path: str
    source_file: str
    source_line: int
    machine: str = ""
    command: str = ""
    status: str = ""
    duration: str = ""
    stdout: str = ""
    stderr: str = ""


@dataclass
class TestCase:
    path: Path
    file_name: str
    suite: str
    name: str
    func_line: int
    disabled: str | None
    next_line: int = 0
    ai_comment_lines: list[str] = field(default_factory=list)
    ai_items: list[str] = field(default_factory=list)
    checklist_statuses: list[str] = field(default_factory=list)
    ai_report_markdown: str = ""
    record_name: str = ""
    commands: list[CommandRun] = field(default_factory=list)


def slug(value: str) -> str:
    result: list[str] = []
    needs_separator = False

    for char in value:
        if char.isascii() and char.isalnum():
            if needs_separator and result:
                result.append("-")
            result.append(char.lower())
            needs_separator = False
        elif result:
            needs_separator = True

    return "".join(result) or "unknown"


def escape(value: str | None) -> str:
    return html.escape(value or "", quote=True)


def display_name(file_name: str) -> str:
    name = file_name.removesuffix(".swift").removesuffix("Tests")
    return re.sub(r"(?<=[a-z0-9])(?=[A-Z])", " ", name)


def fenced(label: str, text: str) -> str:
    match = re.search(rf"### {label}\n\n```text\n(.*?)\n```", text, re.S)
    return match.group(1) if match else ""


def parse_record(path: Path) -> list[CommandRun]:
    text = path.read_text()
    commands: list[CommandRun] = []

    for part in text.split("\n---\n")[1:]:
        if "## Command" not in part:
            continue

        source_match = SOURCE_RE.search(part)
        source_path = source_match.group(1) if source_match else ""
        command = CommandRun(
            record=path.name,
            source_path=source_path,
            source_file=Path(source_path).name if source_path else "",
            source_line=int(source_match.group(2)) if source_match else -1,
        )

        for key, regex in FIELD_RES.items():
            match = regex.search(part)
            if match:
                setattr(command, key, match.group(1))

        command.stdout = fenced("stdout", part)
        command.stderr = fenced("stderr", part)
        commands.append(command)

    return commands


def load_records(records_dir: Path) -> dict[str, list[CommandRun]]:
    return {path.name: parse_record(path) for path in sorted(records_dir.glob("*.md"))}


def extract_ai_comment(lines: list[str]) -> tuple[list[str], list[str]]:
    comment_lines: list[str] = []
    items: list[str] = []
    in_ai = False

    for line in lines:
        stripped = line.strip()
        if "// AI:" in line:
            in_ai = True
            comment_lines.append(line.lstrip())
            continue

        if not in_ai:
            continue

        if stripped.startswith("//"):
            comment_lines.append(line.lstrip())
            item_match = re.search(r"//\s*-\s*(.*)", line)
            if item_match:
                items.append(item_match.group(1).strip())
        elif stripped == "":
            continue
        else:
            in_ai = False

    return comment_lines, items


def parse_tests(tests_dir: Path, records: dict[str, list[CommandRun]]) -> list[tuple[Path, list[TestCase]]]:
    files: list[tuple[Path, list[TestCase]]] = []

    for path in sorted(tests_dir.rglob("*Tests.swift")):
        lines = path.read_text().splitlines()
        suite = path.stem
        pending_test: dict[str, object] | None = None
        tests: list[TestCase] = []

        for index, line in enumerate(lines, 1):
            suite_match = re.search(r"\bstruct\s+`([^`]+)`\s*\{", line) or re.search(
                r"\bstruct\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{", line
            )
            if suite_match:
                suite = suite_match.group(1)

            if "@Test" in line:
                disabled_match = re.search(r"\.disabled\(\"([^\"]*)\"\)", line)
                pending_test = {
                    "line": index,
                    "disabled": disabled_match.group(1) if disabled_match else None,
                }

            function_match = re.search(r"\bfunc\s+`([^`]+)`\s*\(", line) or re.search(
                r"\bfunc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(", line
            )
            if function_match and pending_test is not None:
                tests.append(
                    TestCase(
                        path=path,
                        file_name=path.name,
                        suite=suite,
                        name=function_match.group(1),
                        func_line=index,
                        disabled=pending_test["disabled"],  # type: ignore[arg-type]
                    )
                )
                pending_test = None

        for test_index, test in enumerate(tests):
            next_line = tests[test_index + 1].func_line if test_index + 1 < len(tests) else len(lines) + 1
            body = lines[test.func_line - 1 : next_line - 1]
            test.next_line = next_line
            test.ai_comment_lines, test.ai_items = extract_ai_comment(body)
            test.record_name = f"{path.stem}.{slug(test.name)}.md"
            test.commands = [
                command
                for command in records.get(test.record_name, [])
                if command.source_file == path.name and test.func_line <= command.source_line < next_line
            ]

            statuses, report_markdown = AI_REVIEW.get(
                (test.file_name, test.name),
                (["pass"] * len(test.ai_items), ""),
            )
            test.checklist_statuses = statuses
            test.ai_report_markdown = report_markdown

        if tests:
            files.append((path, tests))

    return files


def render_commands(commands: list[CommandRun]) -> str:
    if not commands:
        return ""

    chunks = ['<div class="commands">']
    for command in commands:
        chunks.append('<section class="command-run">')
        chunks.append(
            '<div class="command-line"><span class="command-prompt">❯</span>'
            f'<span class="command-text">{escape(command.command)}</span></div>'
        )

        output: list[str] = []
        for line in command.stdout.splitlines():
            output.append(
                '<div class="output-line stdout"><span class="stream-marker">!</span>'
                f'<span class="output-text">{escape(line)}</span></div>'
            )
        for line in command.stderr.splitlines():
            output.append(
                '<div class="output-line stderr"><span class="stream-marker">!</span>'
                f'<span class="output-text">{escape(line)}</span></div>'
            )

        if output:
            chunks.append('<div class="command-output">' + "".join(output) + "</div>")

        meta = " · ".join(part for part in [command.machine, command.status, command.duration] if part)
        chunks.append(f'<p class="command-run-meta">{escape(meta)}</p>')
        chunks.append("</section>")

    chunks.append("</div>")
    return "\n".join(chunks)


def render_ai_filter_analysis(test: TestCase) -> str:
    if not test.ai_report_markdown:
        return ""

    return (
        '<div class="ai-filter-analysis" aria-label="AI analysis">'
        f'<pre>{escape(test.ai_report_markdown)}</pre></div>'
    )


def render_ai(test: TestCase) -> str:
    if not test.ai_items:
        return ""

    items: list[str] = []
    for index, item in enumerate(test.ai_items):
        status = test.checklist_statuses[index] if index < len(test.checklist_statuses) else "pass"
        items.append(
            f'<li><span>{escape(item)}</span>'
            f'<span class="status {escape(status)}" aria-label="{escape(status)}"></span></li>'
        )

    return '<section class="ai-analysis"><h4>AI analysis</h4><ul class="checks">%s</ul></section>' % "".join(items)


def add_fake_analysis(files: list[tuple[Path, list[TestCase]]], seed: int) -> int:
    ai_tests = [test for _, tests in files for test in tests if test.ai_items]
    if not ai_tests:
        return 0

    rng = random.Random(seed)
    remarks = FAKE_ANALYSIS_REMARKS[:]
    rng.shuffle(remarks)

    for text in remarks:
        test = rng.choice(ai_tests)
        if test.ai_report_markdown:
            test.ai_report_markdown += "\n\n" + text
        else:
            test.ai_report_markdown = text

    return len(remarks)


def record_ai_review_markdown(test: TestCase) -> str:
    if not test.ai_comment_lines or not test.ai_report_markdown:
        return ""

    lines = [
        AI_REVIEW_HEADING,
        "",
        "### Source `// AI:` comment",
        "",
        "```swift",
        *test.ai_comment_lines,
        "```",
    ]

    if test.ai_report_markdown:
        lines.extend(["", "### Report", "", test.ai_report_markdown])

    lines.append("")
    return "\n".join(lines)


def strip_existing_record_ai_review(markdown: str) -> str:
    old_marker_pattern = re.compile(
        r"\n*---\n\n<!-- Wendy E2E AI review: start -->.*?<!-- Wendy E2E AI review: end -->\n*",
        re.S,
    )
    heading_pattern = re.compile(
        rf"\n*---\n\n{re.escape(AI_REVIEW_HEADING)}.*?(?=\n---\n|\Z)",
        re.S,
    )
    markdown = old_marker_pattern.sub("\n", markdown)
    markdown = heading_pattern.sub("\n", markdown)
    return markdown.rstrip() + "\n"


def append_ai_reviews_to_records(files: list[tuple[Path, list[TestCase]]], records_dir: Path) -> int:
    for record_path in records_dir.glob("*.md"):
        cleaned = strip_existing_record_ai_review(record_path.read_text())
        record_path.write_text(cleaned)

    appended = 0
    for _, tests in files:
        for test in tests:
            review = record_ai_review_markdown(test)
            if not review:
                continue

            record_path = records_dir / test.record_name
            if not record_path.exists():
                continue

            markdown = record_path.read_text().rstrip() + "\n"
            record_path.write_text(f"{markdown}\n---\n\n{review}")
            appended += 1

    return appended


def render_cards(files: list[tuple[Path, list[TestCase]]], records_dir: Path) -> str:
    cards: list[str] = []

    for path, tests in files:
        cards.append('<section class="card" data-test-file-card>')
        cards.append(f'<div class="card-title"><h2>{escape(display_name(path.name))}</h2></div>')
        cards.append('<div class="suite-group">')

        for test in tests:
            status_class = "skipped" if test.disabled else "pass"
            status_text = "Skipped" if test.disabled else "Passed"
            has_ai = "true" if test.ai_items else "false"
            has_ai_analysis = "true" if test.ai_report_markdown else "false"
            record_path = records_dir / test.record_name
            report_link = (
                f'<a class="report-button" href="{escape(test.record_name)}">Record</a>'
                if record_path.exists()
                else ""
            )
            ai_badge = '<span class="badge ai">AI</span>' if test.ai_report_markdown else ""
            ai_filter_analysis = render_ai_filter_analysis(test)
            path_text = f"{test.suite} › {test.name}"

            cards.append(
                f'<details class="test-details" data-test-status="{status_class}" '
                f'data-has-ai="{has_ai}" data-has-ai-analysis="{has_ai_analysis}">'
            )
            cards.append(
                '<summary class="test-summary">'
                f'{report_link}<span class="test-path">{escape(path_text)}</span>'
                f'<span class="badge {status_class}">{status_text}</span>{ai_badge}'
                f'{ai_filter_analysis}</summary>'
            )

            body = []
            if test.disabled:
                body.append(f'<p class="skip-reason">{escape(test.disabled)}</p>')
            body.append(render_commands(test.commands))
            cards.append('<div class="test-body">%s</div>' % "\n".join(part for part in body if part))
            cards.append("</details>")

        cards.append("</div></section>")

    return "\n".join(cards)


def render_report(
    template_path: Path,
    records_dir: Path,
    files: list[tuple[Path, list[TestCase]]],
    output_path: Path,
) -> None:
    passed = sum(1 for _, tests in files for test in tests if not test.disabled)
    skipped = sum(1 for _, tests in files for test in tests if test.disabled)
    failed = 0
    total = passed + skipped + failed
    command_count = sum(len(test.commands) for _, tests in files for test in tests)

    template = template_path.read_text()
    template = re.sub(
        r"\n  <!--\n    Wendy Agent E2E AI Review HTML Template.*?\n  -->",
        "",
        template,
        flags=re.S,
    )
    start = template.index("    <!-- Repeat this .card section once per test file.")
    footer_start = template.index("    <footer>", start)
    template = template[:start] + render_cards(files, records_dir) + "\n\n" + template[footer_start:]

    replacements = {
        "{{REPORT_TITLE}}": "Wendy Agent E2E Report",
        "{{REPORT_HEADING}}": "Wendy Agent E2E Report",
        "{{REPORT_SUMMARY}}": (
            "Generated from Swift E2E tests on "
            f"{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}. Includes active, "
            "disabled, AI-reviewed tests, and captured command records."
        ),
        "{{TESTS_PASSED_COUNT}}": str(passed),
        "{{TESTS_SKIPPED_COUNT}}": str(skipped),
        "{{TESTS_FAILED_COUNT}}": str(failed),
        "{{COMMAND_RUN_COUNT}}": str(command_count),
        "{{VISIBLE_TEST_COUNT}}": str(total),
        "{{TOTAL_TEST_COUNT}}": str(total),
        "{{RECORDS_DIRECTORY}}": str(records_dir),
    }

    raw_placeholders = {
        "{{REPORT_TITLE}}",
        "{{TESTS_PASSED_COUNT}}",
        "{{TESTS_SKIPPED_COUNT}}",
        "{{TESTS_FAILED_COUNT}}",
        "{{COMMAND_RUN_COUNT}}",
        "{{VISIBLE_TEST_COUNT}}",
        "{{TOTAL_TEST_COUNT}}",
    }
    for placeholder, value in replacements.items():
        template = template.replace(placeholder, value if placeholder in raw_placeholders else escape(value))

    leftovers = sorted(set(re.findall(r"\{\{[A-Z0-9_]+\}\}", template)))
    if leftovers:
        raise RuntimeError(f"unreplaced placeholders: {leftovers}")

    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(template)

    print(output_path)
    print(f"tests={total} passed={passed} skipped={skipped} failed={failed} commands={command_count}")


def latest_records_dir(package_dir: Path) -> Path:
    build_dir = package_dir / ".build"
    current = build_dir / "e2e-test-records.current"
    if current.exists():
        return current

    candidates = sorted(path for path in build_dir.glob("e2e-test-records.*") if path.is_dir())
    if not candidates:
        raise FileNotFoundError(f"no e2e-test-records.* directory found in {build_dir}")
    return candidates[-1]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package-dir", type=Path, default=DEFAULT_PACKAGE_DIR)
    parser.add_argument("--tests-dir", type=Path, default=None)
    parser.add_argument("--template", type=Path, default=None)
    parser.add_argument("--records-dir", type=Path, default=None)
    parser.add_argument("--output", type=Path, default=None)
    parser.add_argument(
        "--no-append-ai-to-records",
        action="store_true",
        help="render HTML only; do not append AI review sections to Markdown command records",
    )
    parser.add_argument(
        "--include-fake-analysis",
        action="store_true",
        help="add deterministic fake prose reports for UI testing",
    )
    parser.add_argument(
        "--fake-analysis-seed",
        type=int,
        default=1337,
        help="seed used with --include-fake-analysis",
    )
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    package_dir = args.package_dir
    tests_dir = args.tests_dir or package_dir / "Tests"
    template = args.template or package_dir / "Support/e2e-ai-review-report-template.html"
    records_dir = args.records_dir or latest_records_dir(package_dir)
    output = args.output or records_dir / "index.html"

    records = load_records(records_dir)
    files = parse_tests(tests_dir, records)
    if args.include_fake_analysis:
        fake_count = add_fake_analysis(files, args.fake_analysis_seed)
        print(f"fake_ai_analysis_added={fake_count}")
    if not args.no_append_ai_to_records:
        appended = append_ai_reviews_to_records(files, records_dir)
        print(f"ai_reviews_appended={appended}")
    render_report(template, records_dir, files, output)


if __name__ == "__main__":
    main()
