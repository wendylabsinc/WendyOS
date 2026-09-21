"""Integrated GPU policy plus sole physical Unitree I/O owner service."""
from __future__ import annotations

from physical_io.service import BUNDLE, PhysicalProbeRuntime, make_server


def main() -> None:
    # Initialize CycloneDDS before importing Torch.  On the Jetson image,
    # loading Torch first corrupts CycloneDDS domain initialization and glibc
    # terminates the process before any publisher is created.
    physical = PhysicalProbeRuntime()
    runner = None
    server = None
    try:
        from runtime.physical_policy import IntegratedPhysicalPolicyRunner

        # Load and verify the exact checkpoint and perception provider before
        # exposing the run-policy route as usable.
        runner = IntegratedPhysicalPolicyRunner.load(physical, BUNDLE)
        physical.attach_policy_runner(runner)
        server = make_server(physical)
        server.serve_forever()
    finally:
        if runner is not None:
            runner.stop_requested.set()
        try:
            if server is not None:
                server.server_close()
        finally:
            try:
                if runner is not None:
                    runner.close()
            finally:
                physical.close()


if __name__ == "__main__":
    main()
