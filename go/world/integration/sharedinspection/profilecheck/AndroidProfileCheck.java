package dev.wendy.world;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.PosixFilePermissions;

/** Uses the actual compiled client parser and signer, without Android or networking. */
public final class AndroidProfileCheck {
  public static void main(String[] args) throws Exception {
    if (args.length != 3) throw new IllegalArgumentException("profile, request output and timestamp required");
    byte[] raw = Files.readAllBytes(Path.of(args[0]));
    Protocol.Profile profile = new Protocol.Profile(raw);
    if (!profile.readMode.equals("native_inference")
        || !profile.world.audience.equals("sharedinspection-native-viewer")
        || !Json.array(profile.world.grant.get("scopes")).equals(java.util.List.of("read", "evidence-read")))
      throw new IllegalArgumentException("Native read-only profile required");
    byte[] signed = Json.encode(profile.world.request("read", new byte[0], "android-viewer-profile-check",
        Long.parseLong(args[2]), 30000)).getBytes(StandardCharsets.UTF_8);
    Path output = Files.createFile(Path.of(args[1]), PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------")));
    Files.write(output, signed);
    System.out.println(Json.encode(Json.map("client", "android", "read_mode", profile.readMode,
        "scopes", java.util.List.of("read", "evidence-read"), "profile_bytes", raw.length, "profile_sha256", Protocol.sha256(raw),
        "request_bytes", signed.length, "request_sha256", Protocol.sha256(signed))));
  }
}
