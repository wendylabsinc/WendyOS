import Foundation
import WorldClient

@main struct ZoneClientProbe {
  static func main() async {
    do {
      let a = CommandLine.arguments
      guard a.count >= 4 else { throw ZoneError.invalid }
      let credentials = try ZoneCredentials(data: Data(contentsOf: URL(fileURLWithPath: a[2])))
      let client = try ZoneClient(
        credentials: credentials, trustData: Data(contentsOf: URL(fileURLWithPath: a[3])))
      var result: JSONValue
      switch a[1] {
      case "publish":
        guard a.count == 5 else { throw ZoneError.invalid }
        let raw = try JSONValue.decode(Data(contentsOf: URL(fileURLWithPath: a[4])))
        guard let name = raw["name"]?.string, let revision = raw["revision"]?.string,
          let context = raw["context"], let zones = raw["zones"]
        else { throw ZoneError.invalid }
        let selected = try await client.publish(
          context: JSONDecoder().decode(ZoneContext.self, from: context.encoded()), name: name,
          revision: revision,
          zones: JSONDecoder().decode([ZoneRectangle].self, from: zones.encoded()))
        // Preserve the original signed component and payload bytes.
        FileHandle.standardOutput.write(selected.record + Data("\n".utf8))
        return
      case "list":
        result = .array(
          try await client.list().map {
            .object([
              "sha256": .string($0.digest), "source": $0.source, "deleted": .bool($0.deleted),
              "name": .string($0.name),
            ])
          })
      case "delete":
        guard a.count == 5 else { throw ZoneError.invalid }
        let rows = try await client.list()
        guard let row = rows.first(where: { $0.digest == a[4] && $0.source == credentials.source })
        else { throw ZoneError.unavailable }
        try await client.delete(row)
        result = .object(["deleted": .bool(true)])
      default: throw ZoneError.invalid
      }
      FileHandle.standardOutput.write(try result.encoded() + Data("\n".utf8))
    } catch {
      FileHandle.standardError.write(Data((error.localizedDescription + "\n").utf8))
      exit(1)
    }
  }
}
