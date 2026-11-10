# Discover an HTTP service

Use two connected Linux WendyOS devices with Wendy Net enabled.
Run `wendy run --device <publisher-hostname> --detach` from publisher/.
Run `wendy run --device <client-hostname>` from client/ on the other device.
Replace the selectors with hostnames or IP addresses from `wendy discover`.
The client waits up to 60 seconds, resolves the demo, prints its HTTP response, and exits.
Its manifest disables automatic restarts with `restartPolicy: "no"`.

Both apps select their private eth0 IPv4 interface. Advertise container port 8080;
remote discovery returns the mesh address and published port 18080.
Do not add an incoming UDP 5353 mapping or an Avahi entitlement.
The demo TXT marker filters examples; it is not authentication.

Inspect publisher logs if no response arrives. Restart the publisher and rerun
the client to verify discovery after restart. Stop the publisher app when done:
`wendy --device <publisher-hostname> device apps stop com.example.learn-mdns-publisher`.
MDNS_ADDRESS is an optional local-test override; leave it unset on devices.
