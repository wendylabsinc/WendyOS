# Cloud connectivity

WendyOS devices authenticate to Cloud with their enrolled device certificates.
Your CLI and apps use your signed-in account to reach those devices. See
[device certificates](../pki/) for the trust model.

## Network changes

A device can reach Cloud through its own Internet connection or borrow Internet
access through Wendy Net. If the active network address disappears, the agent
reconnects. Cloud v2 also checks whether a different preferred path is stable and
can reach Cloud before moving its connection. Detection, authentication and
retry delays mean a network change is not an instant handover. Existing sessions
can still be interrupted if their underlying path fails.

Older broker connections check for a usable new address when no Cloud client is
actively connected. Their recovery timing can differ from Cloud v2.

## Slow or borrowed Internet connections

A Bluetooth mesh path can carry Cloud traffic, but connection setup and renewal
can take longer than over Ethernet or WiFi. The agent renews its Cloud connection
in the background and retries temporary failures while its current registration
remains valid. A local mesh connection alone does not prove Cloud access works.

If a device is reachable locally but missing from Cloud:

1. Read `wendy net status --device DEVICE_SELECTOR`. For a device
   borrowing Internet, check that roaming is enabled and a gateway is selected.
2. Verify that the gateway has Internet access and sharing enabled. See
   [share an Internet connection](/docs/guides/wendy-net#4-share-an-internet-connection).
3. Leave the devices connected while setup or retries complete, then run
   `wendy cloud discover` and try a real device command through Cloud.

Read the accompanying error if recovery fails. A DNS error identifies a name
resolution problem on the selected network; a certificate error needs the
reported identity, certificate or clock problem corrected. Cloud registration
can take time to recover, particularly after an ownership change; it is not an
immediate takeover mechanism.
