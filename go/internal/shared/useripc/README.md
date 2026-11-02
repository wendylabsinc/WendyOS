# Per-user broker IPC

Unix uses the existing owner-only socket and directory policy. Windows uses
byte-stream named pipes from the existing Microsoft go-winio dependency, with a
stable name derived from the full profile/socket path and current user SID.
The pipe descriptor grants access to that SID and LocalSystem. Remote named-pipe
clients are refused by the library. A client opens with READ_CONTROL and checks
the connected handle's owner and ACL before sending RPC bytes. Durable broker
directories and state files use the same current-user/LocalSystem policy; Unix
permission bits are not treated as Windows ACL enforcement.

The broker retains its existing primary TLS, exact remote identity, endpoint
instance, proof, user leases and stream bounds. IPC setup context ownership ends
when Dial returns. Stopping admission leaves established invocation-owned
connections alive until their leases drain. The legacy direct broker's inherited
parent-file mechanism remains Unix only; explicit operator contexts use IPC
leases on both platforms.

The portable positive test checks a normal private state file, local round trip,
setup-context detachment, listener drain and read deadline. It runs on macOS and
can run on Windows without hardware. Windows code and test executables have been
cross-built; Windows runtime qualification is still required before claiming
support, including detached helper startup, concurrent cold commands, idle
shutdown, profile ownership and credential renewal.

Implementation references:

- [Microsoft named-pipe access rights](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights)
- [Microsoft securable objects and descriptor APIs](https://learn.microsoft.com/en-us/windows/win32/secauthz/securable-objects)
- [Microsoft go-winio pipe implementation](https://github.com/microsoft/go-winio/blob/main/pipe.go)
