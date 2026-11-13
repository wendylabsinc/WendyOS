#!/usr/bin/env python3

import datetime
import hashlib
import importlib.util
import sys
import unittest
from pathlib import Path

SCRIPT_PATH = Path(__file__).with_name("ValidateMacProvisioning.py")
SPEC = importlib.util.spec_from_file_location("validate_mac_provisioning", SCRIPT_PATH)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class ValidateMacProvisioningTests(unittest.TestCase):
    certificate = b"developer-id-certificate"
    signing_identity = hashlib.sha1(certificate).hexdigest()
    now = datetime.datetime(2026, 10, 6, tzinfo=datetime.timezone.utc)

    def profile(self, bundle_identifier: str, kind: str):
        entitlements = {
            "com.apple.application-identifier": f"3YVC792H3S.{bundle_identifier}",
            "com.apple.developer.team-identifier": "3YVC792H3S",
            "com.apple.developer.networking.networkextension": [
                "packet-tunnel-provider-systemextension",
                "app-proxy-provider-systemextension",
                "content-filter-provider-systemextension",
            ],
        }
        if kind == "host":
            entitlements["com.apple.developer.system-extension.install"] = True
            entitlements["com.apple.security.virtualization"] = True
        return {
            "TeamIdentifier": ["3YVC792H3S"],
            "Platform": ["OSX"],
            "ProvisionsAllDevices": True,
            "ExpirationDate": self.now + datetime.timedelta(days=365),
            "DeveloperCertificates": [self.certificate],
            "Entitlements": entitlements,
        }

    def validate(self, profile, bundle_identifier, kind):
        MODULE.validate_profile(
            profile,
            bundle_identifier=bundle_identifier,
            kind=kind,
            signing_identity=self.signing_identity,
            now=self.now,
        )

    def test_accepts_host_developer_id_profile(self):
        self.validate(self.profile("sh.wendy.WendyAgentMac", "host"), "sh.wendy.WendyAgentMac", "host")

    def test_accepts_net_proxy_developer_id_profile(self):
        self.validate(
            self.profile("sh.wendy.WendyAgentMac.NetProxy", "net-proxy"),
            "sh.wendy.WendyAgentMac.NetProxy",
            "net-proxy",
        )

    def test_rejects_wrong_bundle_identifier(self):
        with self.assertRaisesRegex(MODULE.ValidationError, "wrong application identifier"):
            self.validate(
                self.profile("sh.wendy.WendyAgentMac.WendyNet", "net-proxy"),
                "sh.wendy.WendyAgentMac.NetProxy",
                "net-proxy",
            )

    def test_rejects_expired_profile(self):
        profile = self.profile("sh.wendy.WendyAgentMac", "host")
        profile["ExpirationDate"] = self.now
        with self.assertRaisesRegex(MODULE.ValidationError, "expired"):
            self.validate(profile, "sh.wendy.WendyAgentMac", "host")

    def test_rejects_profile_for_other_certificate(self):
        with self.assertRaisesRegex(MODULE.ValidationError, "selected Developer ID certificate"):
            MODULE.validate_profile(
                self.profile("sh.wendy.WendyAgentMac", "host"),
                bundle_identifier="sh.wendy.WendyAgentMac",
                kind="host",
                signing_identity=hashlib.sha1(b"another-certificate").hexdigest(),
                now=self.now,
            )

    def test_rejects_legacy_network_extension_values(self):
        profile = self.profile("sh.wendy.WendyAgentMac", "host")
        profile["Entitlements"]["com.apple.developer.networking.networkextension"] = [
            "packet-tunnel-provider",
            "app-proxy-provider",
        ]
        with self.assertRaisesRegex(MODULE.ValidationError, "Network Extension values"):
            self.validate(profile, "sh.wendy.WendyAgentMac", "host")

    def test_signed_entitlements_may_only_request_needed_network_extensions(self):
        entitlements = {
            "com.apple.developer.networking.networkextension": [
                "packet-tunnel-provider-systemextension",
                "app-proxy-provider-systemextension",
                "content-filter-provider-systemextension",
            ]
        }
        with self.assertRaisesRegex(MODULE.ValidationError, "unexpected values"):
            MODULE.validate_capabilities(
                entitlements,
                kind="net-proxy",
                exact_network=True,
            )


if __name__ == "__main__":
    unittest.main()
