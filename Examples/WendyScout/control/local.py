"""Documented local admin API, never exposed as an arbitrary RPC tool."""

import json, grpc, pathlib, secrets, re
from wendy.agent.services.v1 import (
    wendy_agent_v1_provisioning_service_pb2 as provisioning,
)
from wendy.agent.services.v1 import wendy_agent_v1_container_service_pb2 as containers
from wendy.agent.services.v1 import shared_pb2 as shared
from common import APP, STATE, save


class Local:
    def __init__(self, socket):
        self.channel = grpc.insecure_channel("unix:" + socket)
        self.owner = STATE / "scratch-owner.json"
        if not self.owner.exists():
            save(
                self.owner,
                {"id": APP + ".scratch." + secrets.token_hex(5), "created": False},
            )
        self.ownership = json.loads(self.owner.read_text())
        self.scratch = self.ownership["id"]
        if not re.fullmatch(re.escape(APP) + r"\.scratch\.[a-f0-9]{10}", self.scratch):
            raise ValueError("Invalid scratch ownership record")
        self.fixture = pathlib.Path("/fixture")

    def scratch_state(self):
        listing = self.channel.unary_stream(
            "/wendy.agent.services.v1.WendyContainerService/ListContainers",
            request_serializer=lambda r: r.SerializeToString(),
            response_deserializer=containers.ListContainersResponse.FromString,
        )
        return next(
            (
                shared.AppRunningState.Name(x.container.running_state)
                for x in listing(containers.ListContainersRequest(), timeout=15)
                if x.container.app_name == self.scratch
            ),
            "ABSENT",
        )

    def reset_fixture(self):
        self.cleanup_scratch()
        owner = self.fixture / "owner.json"
        if owner.exists():
            if json.loads(owner.read_text()).get("app") != self.scratch:
                raise ValueError("Fixture volume ownership mismatch")
        else:
            with owner.open("x") as f:
                json.dump({"app": self.scratch}, f)
        for name in ["request.json", "state.json"]:
            p = self.fixture / name
            if p.is_symlink():
                raise ValueError("Fixture symlink refused")
            p.unlink(missing_ok=True)
        self.generation = secrets.token_hex(16)
        save(self.fixture / "request.json", {"generation": self.generation})

    def fixture_starts(self):
        p = self.fixture / "state.json"
        if not p.exists():
            return 0
        if p.is_symlink() or p.stat().st_size > 4096:
            raise ValueError("Invalid fixture state")
        value = json.loads(p.read_text())
        if value.get("generation") != self.generation:
            return 0
        count = value.get("starts")
        if type(count) is not int or not 1 <= count <= 1000000:
            raise ValueError("Invalid fixture counter")
        return count

    def call(self, service, method, request, response):
        return self.channel.unary_unary(
            "/wendy.agent.services.v1." + service + "/" + method,
            request_serializer=lambda x: x.SerializeToString(),
            response_deserializer=response.FromString,
        )(request, timeout=15)

    def verify_development(self):
        r = self.call(
            "WendyProvisioningService",
            "IsProvisioned",
            provisioning.IsProvisionedRequest(),
            provisioning.IsProvisionedResponse,
        )
        if r.WhichOneof(
            "response"
        ) != "provisioned" or r.provisioned.cloud_host not in {
            "api.dev.wendy.sh",
            "api.dev.wendy.sh:443",
        }:
            raise RuntimeError(
                "Local device is not enrolled in the approved Development cloud; refusing testing"
            )
        # IDs remain inside this trusted process and are never sent to the model/publisher.
        return {
            "environment": "development",
            "connection": "documented local admin socket",
            "self_identified": True,
        }

    def create_scratch(self):
        app = self.scratch
        if self.ownership["created"]:
            return {"status": "already-created", "scratch": "<Scout-owned scratch ID>"}
        listing = self.channel.unary_stream(
            "/wendy.agent.services.v1.WendyContainerService/ListContainers",
            request_serializer=lambda r: r.SerializeToString(),
            response_deserializer=containers.ListContainersResponse.FromString,
        )
        if any(
            x.container.app_name == app
            for x in listing(containers.ListContainersRequest(), timeout=15)
        ):
            raise ValueError(
                "Scratch ID collision; refusing to replace an existing app"
            )
        cfg = {
            "appId": app,
            "version": "0.1.0",
            "platform": "linux",
            "entitlements": [
                {"type": "network", "mode": "none"},
                {"type": "persist", "name": "wendy-scout-ipc", "path": "/ipc"},
                {"type": "persist", "name": "wendy-scout-fixture", "path": "/fixture"},
            ],
            "resources": {"memory": "64Mi", "pids": 8},
        }
        req = containers.CreateContainerRequest(
            image_name="localhost:5000/" + APP + "-worker:latest",
            app_name=app,
            app_config=json.dumps(cfg).encode(),
            restart_policy=shared.RestartPolicy(mode=2),
            user_args=["--scratch"],
        )
        self.ownership["created"] = True
        save(self.owner, self.ownership)  # pre-record crash recovery ownership
        r = self.call(
            "WendyContainerService",
            "CreateContainer",
            req,
            containers.CreateContainerResponse,
        )
        return {
            "scratch": "<Scout-owned scratch ID>",
            "image_digest": r.image_digest,
            "status": "created",
            "connection": "local admin API",
        }

    def cleanup_scratch(self):
        if not self.ownership["created"]:
            return
        try:
            self.call(
                "WendyContainerService",
                "DeleteContainer",
                containers.DeleteContainerRequest(app_name=self.scratch),
                containers.DeleteContainerResponse,
            )
        except grpc.RpcError as e:
            if e.code() != grpc.StatusCode.NOT_FOUND:
                raise

        self.ownership["created"] = False
        save(self.owner, self.ownership)
