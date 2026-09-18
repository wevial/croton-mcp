#!/usr/bin/env python3
"""Offline structural witness, not validation of upstream behavior or citations.

The Markdown record uses closed sections/tables and verdict-dependent fields.
Only the standard library is needed; no CLI, subprocess, network or account access.
"""

import argparse
import copy
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
DOC = ROOT / "docs/design/0004-drive-cli-source-feasibility.md"
PIN = ROOT / "internal/drivecli/client.go"
TITLE = "# Pinned Drive CLI bounded-source feasibility"
SECTIONS = (
    "Pin", "Verdict", "Evidence", "Applicability", "Contract", "Blockers",
    "Evidence boundaries", "Next step", "Investigation",
)
REQUIREMENTS = ("source command/flags", "pre-copy writes", "interruption", "reaping")
LIMITATIONS = (
    "Downloads remain disabled. draft09 is not enabled by this ticket.",
    "Synthetic evidence does not prove real-CLI behavior.",
    "Post-download size checks and size polling are not proof of an enforced transfer cap.",
    "The verifier checks structure and pin agreement; human review evaluates cited evidence.",
)
CONTRACT = {
    "byte bounds": "Enforce the configured byte cap before destination writes and bound source staging independently.",
    "time bounds": "Enforce the configured deadline, interrupt blocked reads, terminate the subprocess and reap it before returning.",
    "cleanup": "Close source descriptors and remove partial output on failure; report cleanup failures.",
    "destination": "Use transactional output descriptors without reopening a validated destination pathname.",
}
BLOCKED_CONTRACT = "None; no supported integration is claimed."
IMMUTABLE = re.compile(r"https://[^\s/]+/[^\s]+/(?:commit|blob)/[0-9a-f]{40}(?:/[^\s]+)?")


class Invalid(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise Invalid(message)


def current_pin(source):
    values = []
    for name in ("pinnedCLIVersion", "versionBannerPrefix"):
        matches = re.findall(r'^\s*' + name + r'\s*=\s*"([^"\n]+)"\s*$', source, re.M)
        require(len(matches) == 1, f"version contract: expected one literal {name}")
        values.append(matches[0])

    return {"version": values[0], "banner": values[1] + values[0], "handshake": "version"}


def parse(text):
    require("<!--" not in text and "```" not in text and "~~~" not in text,
            "record: comments and fenced examples are not evidence fields")
    lines = text.strip().splitlines()
    require(lines and lines[0] == TITLE, f"record: expected title {TITLE}")
    sections = {}
    current = None
    for line in lines[1:]:
        if line.startswith("#"):
            require(line.startswith("## "), "record: only named level-two sections are allowed")
            current = line[3:]
            require(current in SECTIONS and current not in sections,
                    f"record: unknown or duplicate section {current}")
            sections[current] = []
        elif current is None:
            require(not line.strip(), "record: unexpected text before Pin")
        else:
            sections[current].append(line)

    require(tuple(sections) == SECTIONS, "record: expected ordered sections: " + ", ".join(SECTIONS))
    return {name: "\n".join(body).strip() for name, body in sections.items()}


def table(body, header, label):
    rows = []
    for line in body.splitlines():
        require(line.startswith("| ") and line.endswith(" |"), f"{label}: expected Markdown table rows")
        cells = [cell.strip() for cell in line[1:-1].split("|")]
        require(len(cells) == len(header) and all(cells), f"{label}: empty cell or wrong column count")
        rows.append(cells)

    require(len(rows) >= 3 and rows[0] == header, f"{label}: expected header {header} and data")
    require(all(re.fullmatch(r"-+", cell) for cell in rows[1]), f"{label}: invalid table separator")
    keys = [row[0] for row in rows[2:]]
    require(len(keys) == len(set(keys)), f"{label}: duplicate row")
    return {row[0]: row[1:] for row in rows[2:]}


def nonempty(value, label):
    require(bool(value.strip()) and value.strip().lower() not in {"none", "none.", "n/a", "unknown", "unavailable", "tbd", "-"},
            f"{label}: provide concrete evidence, unmet requirement or next action")


def validate(sections, pin):
    identity = table(sections["Pin"], ["Field", "Value"], "Pin")
    require(identity == {key: [value] for key, value in pin.items()},
            "Pin: version, exact banner and handshake must match internal/drivecli/client.go")
    verdict = sections["Verdict"]
    require(verdict in {"supported", "blocked"}, "Verdict: must be exactly supported or blocked")
    evidence = table(sections["Evidence"],
                     ["Requirement", "Kind", "Immutable upstream reference", "Source/manual location", "Exact command/flags", "Finding"],
                     "Evidence")
    require(set(evidence) == set(REQUIREMENTS), "Evidence: require source command/flags, pre-copy writes, interruption and reaping")
    for requirement, (kind, reference, location, command, finding) in evidence.items():
        require(kind in {"real-cli", "synthetic", "missing"}, f"Evidence {requirement}: invalid kind")
        nonempty(finding, f"Evidence {requirement} finding")
        if verdict == "supported":
            require(kind == "real-cli", f"Evidence {requirement}: supported requires real-cli evidence")
        if kind == "real-cli":
            require(IMMUTABLE.fullmatch(reference), f"Evidence {requirement}: require immutable upstream URL with full commit hash")
            for label, value in (("location", location), ("command/flags", command)):
                nonempty(value, f"Evidence {requirement} {label}")
        else:
            require(reference == "unavailable", f"Evidence {requirement}: non-upstream evidence must use unavailable reference")

    platforms = table(sections["Applicability"], ["Platform", "Status", "Assessment"], "Applicability")
    require(set(platforms) == {"Linux", "macOS"}, "Applicability: require Linux and macOS")
    for platform, (status, assessment) in platforms.items():
        require(status == verdict, f"Applicability {platform}: status must agree with verdict")
        nonempty(assessment, f"Applicability {platform}")

    if verdict == "blocked":
        require(sections["Contract"] == BLOCKED_CONTRACT, "Contract: blocked verdict must disclaim supported integration")
        blockers = table(sections["Blockers"], ["Requirement", "Unmet requirement or unavailable artifact"], "Blockers")
        for requirement, (detail,) in blockers.items():
            require(requirement in REQUIREMENTS, f"Blockers: unknown requirement {requirement}")
            nonempty(detail, f"Blockers {requirement}")
    else:
        require(sections["Blockers"] == "None.", "Blockers: supported verdict requires None.")
        contract = table(sections["Contract"], ["Control", "Requirement", "Proposed mechanism"], "Contract")
        require(set(contract) == set(CONTRACT), "Contract: require byte/time bounds, cleanup and destination controls")
        for control, (requirement, mechanism) in contract.items():
            require(requirement == CONTRACT[control], f"Contract {control}: required control statement differs")
            nonempty(mechanism, f"Contract {control} proposed mechanism")

    require(sections["Evidence boundaries"] == "\n\n".join(LIMITATIONS),
            "Evidence boundaries: require all four exact limitations without qualifications")
    nonempty(sections["Next step"], "Next step")
    nonempty(sections["Investigation"], "Investigation")


def render_table(header, rows):
    return "\n".join("| " + " | ".join(row) + " |" for row in [header, ["---"] * len(header), *rows])


def self_test():
    # Invented .test references are parser fixtures, never upstream evidence.
    pin = current_pin('pinnedCLIVersion = "0.8.0"\nversionBannerPrefix = "Proton Drive CLI cli-drive@"')
    supported = dict.fromkeys(SECTIONS, "Fixture only.")
    supported.update({
        "Pin": render_table(["Field", "Value"], [[key, value] for key, value in pin.items()]),
        "Verdict": "supported",
        "Evidence": render_table(
            ["Requirement", "Kind", "Immutable upstream reference", "Source/manual location", "Exact command/flags", "Finding"],
            [[key, "real-cli", "https://upstream.test/cli/blob/" + "a" * 40 + "/source.ts",
              "source.ts:1 fixture", "fixture-cli stream --stdout", "Invented fixture evidence."] for key in REQUIREMENTS]),
        "Applicability": render_table(["Platform", "Status", "Assessment"],
                                      [[p, "supported", "Fixture applicability."] for p in ("Linux", "macOS")]),
        "Contract": render_table(["Control", "Requirement", "Proposed mechanism"],
                                 [[k, v, "Invented fixture mechanism."] for k, v in CONTRACT.items()]),
        "Blockers": "None.",
        "Evidence boundaries": "\n\n".join(LIMITATIONS),
    })
    blocked = copy.deepcopy(supported)
    blocked.update({
        "Verdict": "blocked", "Contract": BLOCKED_CONTRACT,
        "Applicability": supported["Applicability"].replace("supported", "blocked"),
        "Blockers": render_table(["Requirement", "Unmet requirement or unavailable artifact"],
                                 [["source command/flags", "Missing immutable source artifact for stdout mode."]]),
    })
    cases = [("valid supported", supported, True), ("valid blocked", blocked, True)]
    missing_artifact = copy.deepcopy(blocked)
    missing_artifact["Evidence"] = missing_artifact["Evidence"].replace(
        "real-cli | https://upstream.test/cli/blob/" + "a" * 40 + "/source.ts",
        "missing | unavailable",
    )
    cases.append(("valid blocked with unavailable artifacts", missing_artifact, True))

    def negative(name, base, section, value):
        fixture = copy.deepcopy(base)
        fixture[section] = value
        cases.append((name, fixture, False))

    negative("missing pin", blocked, "Pin", "")
    negative("pin mismatch", blocked, "Pin", blocked["Pin"].replace("0.8.0", "0.8.1"))
    negative("invalid verdict", blocked, "Verdict", "maybe")
    negative("supported lacking cancellation evidence", supported, "Evidence",
             "\n".join(line for line in supported["Evidence"].splitlines() if not line.startswith("| interruption |")))
    negative("supported empty cancellation evidence", supported, "Evidence",
             "\n".join(line.replace("Invented fixture evidence.", " ") if line.startswith("| interruption |") else line
                       for line in supported["Evidence"].splitlines()))
    negative("supported missing reaping evidence", supported, "Evidence",
             "\n".join(line for line in supported["Evidence"].splitlines() if not line.startswith("| reaping |")))
    negative("blocked lacking blocker", blocked, "Blockers", "None.")
    negative("blocked claims integration", blocked, "Contract", supported["Contract"])
    negative("mutable upstream reference", supported, "Evidence", supported["Evidence"].replace("a" * 40, "main"))
    negative("synthetic cannot support", supported, "Evidence", supported["Evidence"].replace("real-cli", "synthetic"))
    negative("missing macOS", blocked, "Applicability",
             "\n".join(line for line in blocked["Applicability"].splitlines() if not line.startswith("| macOS |")))
    negative("missing time bound", supported, "Contract",
             "\n".join(line for line in supported["Contract"].splitlines() if not line.startswith("| time bounds |")))
    negative("missing limitations", blocked, "Evidence boundaries", "")
    negative("missing next step", blocked, "Next step", "")
    negative("duplicate evidence", blocked, "Evidence", blocked["Evidence"] + "\n" + blocked["Evidence"].splitlines()[-1])
    negative("empty blocker", blocked, "Blockers", blocked["Blockers"].replace("Missing immutable source artifact for stdout mode.", " "))
    require(len(cases) > 0, "self-test: zero cases")
    failures = []
    for name, fixture, expected in cases:
        text = TITLE + "\n\n" + "\n\n".join(f"## {key}\n\n{value}" for key, value in fixture.items())
        try:
            validate(parse(text), pin)
            passed = True
        except Invalid:
            passed = False
        if passed != expected:
            failures.append(name)

    print(f"self-test: executed {len(cases)} cases ({sum(case[2] for case in cases)} positive, {sum(not case[2] for case in cases)} negative)")
    require(not failures, "self-test: unexpected outcomes: " + ", ".join(failures))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="run in-memory positive and negative fixtures")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
        else:
            validate(parse(DOC.read_text(encoding="utf-8")), current_pin(PIN.read_text(encoding="utf-8")))
    except (Invalid, OSError, UnicodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1

    print("Drive source feasibility OK (offline structural witness only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
