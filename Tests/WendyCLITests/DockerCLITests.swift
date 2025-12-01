import Foundation
import Testing
@testable import wendy

@Suite("DockerCLI Tests")
struct DockerCLITests {

    @Suite("SubprocessError Tests")
    struct SubprocessErrorTests {

        @Test("Error description includes termination reason")
        func errorDescriptionIncludesTerminationReason() {
            let error = DockerCLI.SubprocessError.nonZeroExit(
                command: "docker build -t myapp .",
                exitCode: 1,
                terminationReason: "exited(1)",
                output: "",
                error: "permission denied"
            )

            let description = error.localizedDescription
            #expect(description.contains("exited(1)"))
            #expect(description.contains("docker build -t myapp ."))
            #expect(description.contains("permission denied"))
        }

        @Test("Error description handles unhandled exception")
        func errorDescriptionHandlesUnhandledException() {
            let error = DockerCLI.SubprocessError.nonZeroExit(
                command: "docker buildx build",
                exitCode: 42,
                terminationReason: "unhandledException(42)",
                output: "",
                error: "buildkit crashed"
            )

            let description = error.localizedDescription
            #expect(description.contains("unhandledException(42)"))
            #expect(description.contains("buildkit crashed"))
        }

        @Test("Error description includes output when present")
        func errorDescriptionIncludesOutput() {
            let error = DockerCLI.SubprocessError.nonZeroExit(
                command: "docker push",
                exitCode: 1,
                terminationReason: "exited(1)",
                output: "Step 1/5 : FROM alpine\nStep 2/5 : COPY app /app",
                error: "failed to push"
            )

            let description = error.localizedDescription
            #expect(description.contains("Step 1/5"))
            #expect(description.contains("Step 2/5"))
            #expect(description.contains("failed to push"))
        }

        @Test("Error description format is consistent")
        func errorDescriptionFormatIsConsistent() {
            let error = DockerCLI.SubprocessError.nonZeroExit(
                command: "test command",
                exitCode: 127,
                terminationReason: "exited(127)",
                output: "some output",
                error: "some error"
            )

            let description = error.localizedDescription
            // Format should be: "Command '<cmd>' failed with <reason>: <error>\n\n<output>"
            #expect(description.hasPrefix("Command 'test command' failed with exited(127):"))
        }
    }

    @Suite("Builder Name Generation")
    struct BuilderNameTests {

        @Test("Builder name includes port number")
        func builderNameIncludesPort() {
            let cli = DockerCLI()
            #expect(cli.builderName(forPort: 5000) == "wendy-builder-5000")
            #expect(cli.builderName(forPort: 8080) == "wendy-builder-8080")
            #expect(cli.builderName(forPort: 50053) == "wendy-builder-50053")
        }

        @Test("Builder name is deterministic for same port")
        func builderNameIsDeterministic() {
            let cli = DockerCLI()
            let name1 = cli.builderName(forPort: 5000)
            let name2 = cli.builderName(forPort: 5000)
            #expect(name1 == name2)
        }

        @Test("Different ports generate different builder names")
        func differentPortsGenerateDifferentNames() {
            let cli = DockerCLI()
            let name1 = cli.builderName(forPort: 5000)
            let name2 = cli.builderName(forPort: 5001)
            #expect(name1 != name2)
        }
    }
}
