/// The JSON object `wendy project entitlements add` and `remove` print on
/// stdout in JSON mode. Only the fields the tests check are decoded.
struct ProjectEditResult: Codable {
    let saved: Bool
}
