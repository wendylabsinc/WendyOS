# Authenticated application streams

`Dial` opens the existing `WendyMeshService.MeshDial` RPC on a caller-owned,
already authenticated agent connection. The operator broker can carry that
connection across routed Wendy Net hops. Application bytes stay inside the
same end-to-end agent TLS connection; no second TLS layer is created.

The first client frame is the existing port-open request. The server authenticates
a canonical same-tenant device or operator and calls its existing running-app
ingress authorizer. It sends an empty `MeshDialData` ready frame only after the
actual authorized destination connection opens. Port admission is not inferred
from a discovery record. Each server allows eight admitted app streams. TCP
payload frames emitted by these adapters are at most 16 KiB.

The returned socket owns one RPC stream, not its shared agent connection.
Read/write use separate bounded pipes so either side can half-close independently,
with actual per-direction deadlines and backpressure. Setup cancellation stops
an outstanding open; a returned socket survives cancellation of its setup context
and is released by Close. No transfer is replayed after a transport loss.

Positive tests cover a 128 KiB upload/echo, a peer read-half-close followed by
continued upload, pending-open cancellation and setup-context detachment. Agent
integration tests exercise the production TLS server/client with primary P-256
and ML-DSA-65 operator certificates, normal partial local TCP writes, one actual
TLS connection, response drain, and an agent RPC on that same connection after
the application stream closes. These are host qualifications. Native Lite VIP
app-stream forwarding and production lab workflows are separate coverage.
