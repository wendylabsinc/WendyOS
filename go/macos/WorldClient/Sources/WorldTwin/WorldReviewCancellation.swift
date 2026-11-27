import Foundation

enum WorldReviewCancellation {
    static func write(to output: URL) {
        // An approval written before close remains authoritative and unchanged.
        try? Data("{\"action\":\"cancel\"}".utf8).write(to: output, options: .withoutOverwriting)
    }
}
