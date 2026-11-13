"""Reduce an isolated E2E auth copy to a currently valid held certificate."""
import json
import os
from pathlib import Path
import ssl
import subprocess
import tempfile
import time


class LANFixtureError(ValueError):
    pass


def prepare_config(path):
    path = Path(path).resolve()
    sandbox = path.parent.parent
    if (path.name != "config.json" or path.parent.name != ".wendy"
            or sandbox.name != "home" or sandbox.parent.name != "cli"):
        raise LANFixtureError("LAN auth preparation requires an isolated cli/home sandbox")
    override = os.environ.get("WENDY_CONFIG_DIR")
    if override and Path(override).resolve() != path.parent:
        raise LANFixtureError("LAN auth fixture refuses a config override outside its isolated sandbox")
    config = json.loads(path.read_text())
    entries = config.get("auth", [])
    if len(entries) != 1 or len(entries[0].get("certificates", [])) != 1:
        raise LANFixtureError("LAN fixture requires exactly one auth context and one held certificate")
    entry = entries[0]
    certificate = entry["certificates"][0]
    cert_fields = {"pemCertificate", "pemCertificateChain", "pemPrivateKey",
                   "organizationId", "userId", "assetId", "principalUri"}
    held = {key: value for key, value in certificate.items() if key in cert_fields}
    clean_entry = {key: entry[key] for key in ("name", "cloudGRPC") if key in entry}
    clean_entry["certificates"] = [held]
    clean = {key: config[key] for key in ("currentContext", "devicePins") if key in config}
    clean.update(auth=[clean_entry], analytics={"enabled": False})
    # Whitelisting removes access/refresh tokens, DPoP and service-account keys,
    # registry credentials, renewal endpoints and any future auth secret fields.
    # Write before validation so even a rejected sandbox contains no rotating token.
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
            temporary = Path(output.name)
            os.chmod(temporary, 0o600)
            json.dump(clean, output)
            output.write("\n")
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    pem = held.get("pemCertificate", "")
    if not pem or not held.get("pemPrivateKey"):
        raise LANFixtureError("LAN fixture requires a held certificate and private key")
    dates = subprocess.run(
        ["openssl", "x509", "-noout", "-startdate", "-enddate"],
        input=pem, text=True, capture_output=True, check=False,
    )
    if dates.returncode:
        raise LANFixtureError("LAN fixture certificate could not be parsed")
    try:
        values = dict(line.split("=", 1) for line in dates.stdout.splitlines())
        starts = ssl.cert_time_to_seconds(values["notBefore"])
        expires = ssl.cert_time_to_seconds(values["notAfter"])
    except (KeyError, ValueError):
        raise LANFixtureError("LAN fixture certificate validity could not be parsed") from None
    now = time.time()
    if not starts <= now < expires:
        raise LANFixtureError("LAN fixture held certificate is expired or not yet valid; stop and ask root")


if __name__ == "__main__":
    try:
        prepare_config(Path.home() / ".wendy" / "config.json")
    except (LANFixtureError, OSError, ValueError) as error:
        raise SystemExit(str(error)) from None
    print("LAN fixture: valid held certificate; renewable credentials removed")
