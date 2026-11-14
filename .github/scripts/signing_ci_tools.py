#!/usr/bin/env python3
"""Recording native-tool boundaries for signing_ci_test.py; never call macOS tools."""

import fcntl
import json
import os
from pathlib import Path
import shutil
import signal
import sys
import zipfile


def main():
    tool, *args = sys.argv[1:]
    state_path = Path(os.environ["SIGNING_FIXTURE_STATE"])
    # Build.sh runs xcodebuild and its formatter as a pipeline. Serialize the
    # recording state across those real concurrent child processes.
    state_lock = open(str(state_path) + ".lock", "a")
    fcntl.flock(state_lock.fileno(), fcntl.LOCK_EX)
    state = json.loads(state_path.read_text())
    job = os.environ["SIGNING_FIXTURE_JOB"]
    keychain = str(Path(os.environ["RUNNER_TEMP"]) / "wendy-signing.keychain-db")
    fingerprint = os.environ["SIGNING_FIXTURE_FINGERPRINT"]
    operation = tool
    state["calls"].append([job, tool, args])

    def option(name):
        if name not in args:
            raise AssertionError(f"{tool} omitted {name}")
        return args[args.index(name) + 1]

    def scoped(name="--keychain"):
        selected = option(name)
        assert selected == keychain, f"{tool} selected another job's keychain: {selected}"
        assert selected in state["keychains"], "keychain was not created"
        return state["keychains"][selected]

    if tool == "openssl":
        assert args == ["rand", "-hex", "24"]
        print(fingerprint + "12345678")
    elif tool == "curl":
        assert args[1] == "https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer"
        Path(option("-o")).write_bytes(b"fixture public intermediate")
    elif tool == "shasum":
        assert Path(args[-1]).read_bytes() == b"fixture public intermediate"
        digest = "0" * 64 if os.environ.get("SIGNING_FIXTURE_BAD_CA") else "f16cd3c54c7f83cea4bf1a3e6a0819c8aaa8e4a1528fd144715f350643d2df3a"
        print(digest, args[-1])
    elif tool == "security":
        operation += ":" + args[0]
        if args[0] == "create-keychain":
            assert args[-1] == keychain
            assert keychain not in state["keychains"], "previous job's keychain leaked"
            Path(keychain).write_text("fake keychain")
            state["keychains"][keychain] = {"password": option("-p"), "imports": [], "profiles": {}}
        elif args[0] == "delete-keychain":
            assert args[-1] == keychain, "attempted to delete a user's keychain"
            state["keychains"].pop(keychain, None)
            state["search"] = [p for p in state["search"] if p != keychain]
            Path(keychain).unlink(missing_ok=True)
        elif args[0] in ("set-keychain-settings", "unlock-keychain", "set-key-partition-list"):
            assert args[-1] == keychain
            entry = state["keychains"][keychain]
            if args[0] != "set-keychain-settings":
                assert option("-p" if args[0] == "unlock-keychain" else "-k") == entry["password"]
        elif args[0] == "import":
            entry = scoped("-k")
            imported = Path(args[1])
            assert imported.is_file()
            operation += ":" + imported.suffix.removeprefix(".")
            if imported.name == "developer-id-g2-ca.cer":
                operation = "security:import:intermediate"
            else:
                input_name = "DEVELOPER_ID_CERTIFICATE" if imported.suffix == ".cer" else "DEVELOPER_ID_KEY"
                assert imported.read_text() == f"fake {job} {input_name}", "import used another job's certificate or key"
            entry["imports"].append(imported.name)
        elif args[0] == "list-keychains":
            if "-s" in args:
                operation += ":set"
                new_search = args[args.index("-s") + 1:]
                assert all(p in new_search for p in state["search"]), "existing user keychains were removed"
                state["search"] = new_search
            else:
                for path in state["search"]:
                    print(f'    "{path}"')
        elif args[0] == "find-identity":
            assert args[-1] == keychain, "identity lookup was not scoped to this job"
            assert state["keychains"][keychain]["imports"] == [
                "developer-id-g2-ca.cer", "developer-id-application.cer", "developer-id-application.key"
            ], "certificate chain or private key was not imported"
            # The existing login keychain has another certificate with this
            # exact display name. Only this scoped fingerprint is usable.
            print('  1) ' + "D" * 40 + ' "Apple Development: Fixture"')
            print(f'  2) {fingerprint} "Developer ID Application: Duplicate Name (TEAM)"')
            print('  3) ' + "E" * 40 + ' "Developer ID Application: Duplicate Name (TEAM)"')
            print('     3 valid identities found')
        else:
            raise AssertionError(f"unexpected security operation: {args}")
    elif tool == "codesign":
        if "--sign" in args:
            operation += ":sign"
            scoped()
            assert option("--sign") == fingerprint, "signer was a display name or another certificate"
            assert keychain in state["search"], "signing keychain is absent from the user search list"
            assert Path(args[-1]).exists()
        else:
            assert "--verify" in args
    elif tool == "xcrun":
        operation += ":" + ":".join(args[:2])
        if args[:2] == ["notarytool", "store-credentials"]:
            entry = scoped()
            assert option("--apple-id") == os.environ["NOTARY_APPLE_ID"]
            assert option("--team-id") == os.environ["NOTARY_TEAM_ID"]
            assert option("--password") == os.environ["NOTARY_PASSWORD"]
            entry["profiles"][args[2]] = {"owner": job, "password": option("--password")}
        elif args[:2] == ["notarytool", "submit"]:
            entry = scoped()
            profile = entry["profiles"][option("--keychain-profile")]
            assert profile["owner"] == job, "notary submission reused another job's credentials"
            assert profile["password"] == os.environ["NOTARY_PASSWORD"]
            assert Path(args[2]).is_file()
        else:
            assert args[:2] in (["stapler", "staple"], ["stapler", "validate"])
    elif tool == "xcodebuild":
        app = Path(option("-derivedDataPath")) / "Build/Products/Release/WendyAgentMac.app/Contents"
        (app / "Library/SystemExtensions/sh.wendy.WendyAgentMac.NetProxy.systemextension/Contents").mkdir(parents=True)
        (app / "Frameworks/Fixture.framework").mkdir(parents=True)
        (app / "MacOS").mkdir()
        (app / "MacOS/WendyAgentMac").write_text("fixture executable")
    elif tool == "ditto":
        source, dest = map(Path, args[-2:])
        if "-c" in args:
            with zipfile.ZipFile(dest, "w") as archive:
                for item in source.rglob("*"):
                    archive.write(item, item.relative_to(source.parent))
        elif "-x" in args:
            with zipfile.ZipFile(source) as archive:
                archive.extractall(dest)
        else:
            shutil.copytree(source, dest)
    elif tool not in ("spctl", "xcbeautify", "ValidateMacRelease.sh", "SmokeTestMacReleaseLaunch.sh"):
        raise AssertionError(f"unexpected tool {tool}")
    if os.environ.get("SIGNING_FIXTURE_STOP_AT") == operation:
        state.setdefault("stops", []).append([job, operation, os.environ.get("SIGNING_FIXTURE_CANCEL", "0")])
        state_path.write_text(json.dumps(state))
        if os.environ.get("SIGNING_FIXTURE_CANCEL") == "1":
            os.kill(os.getppid(), signal.SIGTERM)
        sys.exit(72)
    state_path.write_text(json.dumps(state))


if __name__ == "__main__":
    main()
