import Foundation

public struct ProfileResolutionContext: Sendable, Hashable {
    public var target: AppConfig.ProfileTarget
    public var os: String?
    public var arch: String?
    public var traits: Set<String>
    public var devicePlatform: String?
    public var deviceHostname: String?

    public init(
        target: AppConfig.ProfileTarget,
        os: String? = nil,
        arch: String? = nil,
        traits: Set<String> = [],
        devicePlatform: String? = nil,
        deviceHostname: String? = nil
    ) {
        self.target = target
        self.os = os
        self.arch = arch
        self.traits = traits
        self.devicePlatform = devicePlatform
        self.deviceHostname = deviceHostname
    }
}

public enum ProfileResolutionError: Error, LocalizedError, Sendable {
    case noProfilesDefined
    case profileNotFound(id: String)
    case noMatchingProfile(target: AppConfig.ProfileTarget, availableProfileIDs: [String])
    case invalidDeviceHostnameRegex(id: String, pattern: String)

    public var errorDescription: String? {
        switch self {
        case .noProfilesDefined:
            return "No profiles are defined in wendy.json."
        case .profileNotFound(let id):
            return "Profile '\(id)' was not found in wendy.json."
        case .noMatchingProfile(let target, let availableProfileIDs):
            if availableProfileIDs.isEmpty {
                return "No matching profile found for target '\(target.rawValue)'."
            }
            return
                "No matching profile found for target '\(target.rawValue)'. Available profiles: \(availableProfileIDs.joined(separator: ", "))"
        case .invalidDeviceHostnameRegex(let id, let pattern):
            return "Profile '\(id)' contains an invalid hostname regex: '\(pattern)'."
        }
    }
}

extension AppConfig {
    public var hasProfiles: Bool {
        !(profiles ?? []).isEmpty
    }

    public func profile(withID id: String) -> Profile? {
        profiles?.first(where: { $0.id == id })
    }

    public func resolveProfile(
        context: ProfileResolutionContext,
        requestedProfileID: String? = nil
    ) throws -> Profile {
        let profiles = self.profiles ?? []
        guard !profiles.isEmpty else {
            throw ProfileResolutionError.noProfilesDefined
        }

        if let requestedProfileID {
            guard let profile = profiles.first(where: { $0.id == requestedProfileID }) else {
                throw ProfileResolutionError.profileNotFound(id: requestedProfileID)
            }
            return profile
        }

        let candidates = try profiles.compactMap { profile -> Candidate? in
            guard let match = try match(profile: profile, context: context) else {
                return nil
            }
            return Candidate(
                profile: profile,
                score: match.score,
                specificity: match.specificity,
                priority: profile.priority ?? 0
            )
        }
        .sorted()

        if let best = candidates.first {
            if let defaultProfile,
                let preferred = candidates.first(where: { candidate in
                    candidate.profile.id == defaultProfile
                        && candidate.score == best.score
                        && candidate.priority == best.priority
                        && candidate.specificity == best.specificity
                })
            {
                return preferred.profile
            }
            return best.profile
        }

        let available =
            profiles
            .filter { $0.when.target == context.target }
            .map(\.id)
            .sorted()
        throw ProfileResolutionError.noMatchingProfile(
            target: context.target,
            availableProfileIDs: available
        )
    }
}

extension AppConfig {
    fileprivate struct Candidate: Comparable {
        let profile: Profile
        let score: Int
        let specificity: Int
        let priority: Int

        static func < (lhs: Candidate, rhs: Candidate) -> Bool {
            // Reverse ordering for score/priority/specificity (higher wins).
            if lhs.score != rhs.score {
                return lhs.score > rhs.score
            }
            if lhs.priority != rhs.priority {
                return lhs.priority > rhs.priority
            }
            if lhs.specificity != rhs.specificity {
                return lhs.specificity > rhs.specificity
            }
            return lhs.profile.id < rhs.profile.id
        }
    }

    fileprivate struct Match {
        let score: Int
        let specificity: Int
    }

    fileprivate func match(profile: Profile, context: ProfileResolutionContext) throws -> Match? {
        guard profile.when.target == context.target else {
            return nil
        }

        var score = 0
        var specificity = 0

        let normalizedContextOS = context.os?.lowercased()
        let normalizedContextArch = context.arch?.lowercased()
        let normalizedTraits = Set(context.traits.map { $0.lowercased() })

        if let os = profile.when.os?.lowercased() {
            guard normalizedContextOS == os else {
                return nil
            }
            score += 100
            specificity += 1
        }

        if let arch = profile.when.arch?.lowercased() {
            guard normalizedContextArch == arch else {
                return nil
            }
            score += 100
            specificity += 1
        }

        if let traits = profile.when.traits, !traits.isEmpty {
            let requiredTraits = Set(traits.map { $0.lowercased() })
            guard requiredTraits.isSubset(of: normalizedTraits) else {
                return nil
            }
            score += traits.count * 25
            specificity += 1 + traits.count
        }

        if let device = profile.when.device {
            if let platform = device.platform?.lowercased() {
                guard context.devicePlatform?.lowercased() == platform else {
                    return nil
                }
                score += 50
                specificity += 1
            }

            if let hostnameRegex = device.hostnameRegex {
                guard let hostname = context.deviceHostname else {
                    return nil
                }
                let regex: NSRegularExpression
                do {
                    regex = try NSRegularExpression(pattern: hostnameRegex)
                } catch {
                    throw ProfileResolutionError.invalidDeviceHostnameRegex(
                        id: profile.id,
                        pattern: hostnameRegex
                    )
                }
                let range = NSRange(hostname.startIndex..<hostname.endIndex, in: hostname)
                guard regex.firstMatch(in: hostname, range: range) != nil else {
                    return nil
                }
                score += 50
                specificity += 1
            }
        }

        return Match(score: score, specificity: specificity)
    }
}
