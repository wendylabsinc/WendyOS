"""Dependency-free constants for the installed Stage 2 policy ABI."""

POLICY_MANIFEST = "POLICY.json"
POLICY_SCHEMA = "wendy.g1.reference-residual-runtime-bundle.v1"
RUNTIME_ABI = "g1-reference-residual-gru-316x15-40hz-v1"
EXPECTED_ARCHITECTURE = "reference-residual-gru-v1"
EXPECTED_OWNED_INDICES = [12, *range(29, 43)]
EXPECTED_SOURCE_SHA256 = {
    "source/recurrent_bc/baseline/visual_policy.py": "109b7f0e060ffd3ecdf214a4fc938238404ecdf89394ab76b823837642c1c432",
    "source/recurrent_bc/recurrent_policy.py": "da0832f5456a50554eaffcec456cb1081b2678325daf6c1dc77f4717c73285d8",
    "source/reference_rl/recurrent_residual.py": "cf905a1a4db80e1a93455d33cb90974c0fd77ad482674e739d9debba8aa45800",
    "source/reference_rl/policy.py": "9e617c72e84ebc1a767939ed924e9c0fa7e943049219a1bba3b9da36f5c50227",
    "source/reference_rl/sensor_contract.py": "9b4325116a7610a77ec0b416b456c316c098657f283077733cdfd99c7d77042b",
}
