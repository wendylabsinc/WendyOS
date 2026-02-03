import Foundation
import Testing

@testable import wendy

@Suite("PythonDockerfileGenerator Tests")
struct PythonDockerfileGeneratorTests {

    // MARK: - Python Version Detection Tests

    @Suite("Python Version Detection")
    struct PythonVersionDetectionTests {

        @Test("Defaults to 3.12 when no version files exist")
        func testDefaultVersion() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            // Create empty requirements.txt
            try "flask\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.getPythonVersion() == "3.12")
            #expect(generator.detectPythonVersion() == nil)
        }

        @Test("Reads version from .python-version file")
        func testPythonVersionFile() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "3.11.4\n".write(
                to: tempDir.appendingPathComponent(".python-version"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.getPythonVersion() == "3.11")
        }

        @Test("Reads version from pyproject.toml requires-python")
        func testPyprojectTomlVersion() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let pyproject = """
                [project]
                name = "myapp"
                requires-python = ">=3.10"
                """
            try pyproject.write(
                to: tempDir.appendingPathComponent("pyproject.toml"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.getPythonVersion() == "3.10")
        }

        @Test(".python-version takes precedence over pyproject.toml")
        func testVersionPrecedence() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "3.12\n".write(
                to: tempDir.appendingPathComponent(".python-version"),
                atomically: true,
                encoding: .utf8
            )

            let pyproject = """
                [project]
                requires-python = ">=3.10"
                """
            try pyproject.write(
                to: tempDir.appendingPathComponent("pyproject.toml"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.getPythonVersion() == "3.12")
        }
    }

    // MARK: - Entry Point Detection Tests

    @Suite("Entry Point Detection")
    struct EntryPointDetectionTests {

        @Test("Detects main.py as entry point")
        func testDetectsMainPy() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "print('hello')".write(
                to: tempDir.appendingPathComponent("main.py"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectEntryPoint() == "main.py")
        }

        @Test("Detects app.py as entry point")
        func testDetectsAppPy() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "print('hello')".write(
                to: tempDir.appendingPathComponent("app.py"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectEntryPoint() == "app.py")
        }

        @Test("main.py takes precedence over app.py")
        func testMainPyPrecedence() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "print('main')".write(
                to: tempDir.appendingPathComponent("main.py"),
                atomically: true,
                encoding: .utf8
            )
            try "print('app')".write(
                to: tempDir.appendingPathComponent("app.py"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectEntryPoint() == "main.py")
        }

        @Test("Detects manage.py for Django projects")
        func testDetectsManagePy() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "#!/usr/bin/env python".write(
                to: tempDir.appendingPathComponent("manage.py"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectEntryPoint() == "manage.py")
        }

        @Test("Returns nil when no Python files exist")
        func testNoEntryPoint() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectEntryPoint() == nil)
        }

        @Test("Lists Python files excluding __init__.py")
        func testListPythonFiles() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "".write(
                to: tempDir.appendingPathComponent("server.py"),
                atomically: true,
                encoding: .utf8
            )
            try "".write(
                to: tempDir.appendingPathComponent("utils.py"),
                atomically: true,
                encoding: .utf8
            )
            try "".write(
                to: tempDir.appendingPathComponent("__init__.py"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let files = generator.listPythonFiles()

            #expect(files.contains("server.py"))
            #expect(files.contains("utils.py"))
            #expect(!files.contains("__init__.py"))
        }
    }

    // MARK: - Framework Detection Tests

    @Suite("Framework Detection")
    struct FrameworkDetectionTests {

        @Test("Detects Flask framework")
        func testDetectsFlask() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "flask==2.0.0\nrequests\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectFramework() == .flask)
        }

        @Test("Detects FastAPI framework")
        func testDetectsFastAPI() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "fastapi\nuvicorn\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectFramework() == .fastapi)
        }

        @Test("Detects Django framework")
        func testDetectsDjango() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "Django>=4.0\npsycopg2-binary\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectFramework() == .django)
        }

        @Test("FastAPI takes precedence over uvicorn")
        func testFastAPITakesPrecedence() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "uvicorn\nfastapi\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectFramework() == .fastapi)
        }

        @Test("Returns none when no framework detected")
        func testNoFramework() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "requests\nnumpy\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            #expect(generator.detectFramework() == .none)
        }
    }

    // MARK: - System Dependencies Detection Tests

    @Suite("System Dependencies Detection")
    struct SystemDependenciesTests {

        @Test("Detects psycopg2 system dependencies")
        func testPsycopg2Deps() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "psycopg2>=2.9\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let deps = generator.detectSystemDependencies()
            #expect(deps.contains("libpq-dev"))
        }

        @Test("Detects Pillow system dependencies")
        func testPillowDeps() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "Pillow\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let deps = generator.detectSystemDependencies()
            #expect(deps.contains("libjpeg-dev"))
            #expect(deps.contains("zlib1g-dev"))
            #expect(deps.contains("libpng-dev"))
        }

        @Test("No system deps for binary wheels like psycopg2-binary")
        func testBinaryWheelNoDeps() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "psycopg2-binary\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let deps = generator.detectSystemDependencies()
            #expect(deps.isEmpty)
        }

        @Test("Combines multiple package dependencies")
        func testCombinedDeps() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "psycopg2\nlxml\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let deps = generator.detectSystemDependencies()
            #expect(deps.contains("libpq-dev"))
            #expect(deps.contains("libxml2-dev"))
            #expect(deps.contains("libxslt-dev"))
        }
    }

    // MARK: - Dockerfile Generation Tests

    @Suite("Dockerfile Generation")
    struct DockerfileGenerationTests {

        @Test("Generates valid Dockerfile for Flask app")
        func testFlaskDockerfile() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "flask\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )
            try "3.11\n".write(
                to: tempDir.appendingPathComponent(".python-version"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let dockerfile = generator.generateDockerfile(entryPoint: "app.py")

            #expect(dockerfile.contains("FROM python:3.11"))
            #expect(dockerfile.contains("COPY requirements.txt"))
            #expect(dockerfile.contains("pip install --no-cache-dir -r requirements.txt"))
            #expect(dockerfile.contains("EXPOSE 8000"))
            #expect(dockerfile.contains("flask"))
            #expect(dockerfile.contains("app"))
        }

        @Test("Generates valid Dockerfile for FastAPI app")
        func testFastAPIDockerfile() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "fastapi\nuvicorn\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let dockerfile = generator.generateDockerfile(entryPoint: "main.py")

            #expect(dockerfile.contains("FROM python:3.12"))
            #expect(dockerfile.contains("uvicorn"))
            #expect(dockerfile.contains("main:app"))
        }

        @Test("Generates Dockerfile with system dependencies")
        func testDockerfileWithSystemDeps() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "psycopg2\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let dockerfile = generator.generateDockerfile(entryPoint: "main.py")

            #expect(dockerfile.contains("apt-get update"))
            #expect(dockerfile.contains("libpq-dev"))
        }

        @Test("Generates simple Dockerfile when no framework detected")
        func testSimpleDockerfile() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "requests\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let dockerfile = generator.generateDockerfile(entryPoint: "script.py")

            #expect(dockerfile.contains("FROM python:3.12"))
            #expect(dockerfile.contains(#"CMD ["python", "script.py"]"#))
        }

        @Test("Writes Dockerfile to disk")
        func testWriteDockerfile() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "flask\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            try generator.writeDockerfile(entryPoint: "app.py")

            let dockerfilePath = tempDir.appendingPathComponent("Dockerfile")
            #expect(FileManager.default.fileExists(atPath: dockerfilePath.path))

            let content = try String(contentsOf: dockerfilePath, encoding: .utf8)
            #expect(content.contains("FROM python:"))
        }
    }

    // MARK: - Requirements Parsing Tests

    @Suite("Requirements Parsing")
    struct RequirementsParsingTests {

        @Test("Parses simple requirements")
        func testSimpleParsing() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "flask\nrequests\nnumpy\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let packages = generator.parseRequirements()

            #expect(packages.contains("flask"))
            #expect(packages.contains("requests"))
            #expect(packages.contains("numpy"))
        }

        @Test("Strips version specifiers")
        func testStripsVersions() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "flask>=2.0.0\nrequests==2.28.0\nnumpy~=1.23\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let packages = generator.parseRequirements()

            #expect(packages.contains("flask"))
            #expect(packages.contains("requests"))
            #expect(packages.contains("numpy"))
            #expect(!packages.contains("flask>=2.0.0"))
        }

        @Test("Skips comments and empty lines")
        func testSkipsComments() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try """
            # This is a comment
            flask

            # Another comment
            requests
            """.write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let packages = generator.parseRequirements()

            #expect(packages.count == 2)
            #expect(packages.contains("flask"))
            #expect(packages.contains("requests"))
        }

        @Test("Handles extras syntax")
        func testHandlesExtras() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            try "uvicorn[standard]\ncelery[redis]\n".write(
                to: tempDir.appendingPathComponent("requirements.txt"),
                atomically: true,
                encoding: .utf8
            )

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let packages = generator.parseRequirements()

            #expect(packages.contains("uvicorn"))
            #expect(packages.contains("celery"))
        }
    }

    // MARK: - CMD Generation Tests

    @Suite("CMD Generation")
    struct CMDGenerationTests {

        @Test("Generates Flask CMD")
        func testFlaskCMD() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let cmd = generator.generateCmd(framework: .flask, entryPoint: "app.py")

            #expect(cmd.contains("flask"))
            #expect(cmd.contains("--app"))
            #expect(cmd.contains("app"))
            #expect(cmd.contains("0.0.0.0"))
        }

        @Test("Generates FastAPI CMD")
        func testFastAPICMD() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let cmd = generator.generateCmd(framework: .fastapi, entryPoint: "main.py")

            #expect(cmd.contains("uvicorn"))
            #expect(cmd.contains("main:app"))
            #expect(cmd.contains("0.0.0.0"))
        }

        @Test("Generates Django CMD")
        func testDjangoCMD() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let cmd = generator.generateCmd(framework: .django, entryPoint: "manage.py")

            #expect(cmd.contains("python"))
            #expect(cmd.contains("manage.py"))
            #expect(cmd.contains("runserver"))
        }

        @Test("Generates simple CMD for unknown framework")
        func testSimpleCMD() throws {
            let tempDir = FileManager.default.temporaryDirectory
                .appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: tempDir) }

            let generator = PythonDockerfileGenerator(projectPath: tempDir.path)
            let cmd = generator.generateCmd(framework: .none, entryPoint: "script.py")

            #expect(cmd == #"CMD ["python", "script.py"]"#)
        }
    }
}
