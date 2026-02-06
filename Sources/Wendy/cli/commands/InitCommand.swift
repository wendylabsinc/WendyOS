import AppConfig
import ArgumentParser
import CLIOutput
import Logging
import Subprocess
import SystemPackage

#if canImport(FoundationEssentials)
    import FoundationEssentials
#else
    import Foundation
#endif

struct InitCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "init",
        abstract: "Create a new Wendy project"
    )

    @Option(
        name: .customLong("path"),
        help: "Path where the project should be created (defaults to current directory)"
    )
    var projectPath: String = "."

    @Option(
        name: .customLong("language"),
        help: "Programming language for the project (swift or python)"
    )
    var language: ProjectLanguage = .swift

    private var logger: Logger {
        Logger(label: "sh.wendy.cli.init")
    }

    func run() async throws {
        logger.debug("Initializing new Wendy project", metadata: ["path": .string(projectPath)])
        cliOutput.info("Creating new WendyOS project")

        // Create the directory if it doesn't exist
        let fileManager = FileManager.default
        if !fileManager.fileExists(atPath: projectPath) {
            do {
                try fileManager.createDirectory(
                    atPath: projectPath,
                    withIntermediateDirectories: true
                )
                cliOutput.info("Creating project directory at \(projectPath)")
            } catch {
                throw CLIError.directoryCreationFailed(
                    path: projectPath,
                    reason: error.localizedDescription
                )
            }
        }

        // Initialize project based on selected language
        switch language {
        case .swift:
            try await initializeSwiftProject()
        case .python:
            try await initializePythonProject()
        }

        // Create wendy directory inside the project path
        let wendyDirPath =
            projectPath.hasSuffix("/") ? "\(projectPath)wendy" : "\(projectPath)/wendy"

        do {
            try fileManager.createDirectory(atPath: wendyDirPath, withIntermediateDirectories: true)
        } catch {
            throw CLIError.directoryCreationFailed(
                path: wendyDirPath,
                reason: error.localizedDescription
            )
        }

        // Create default wendy.json configuration file
        try await createDefaultWendyJson(in: projectPath, language: language)
    }

    private func initializeSwiftProject() async throws {
        if FileManager.default.fileExists(atPath: "\(projectPath)/Package.swift") {
            logger.debug("Package.swift already exists, skipping initialization")
            return
        }

        // Run swift package init in the specified directory using bash -c to cd into the directory first
        let result = try await Subprocess.run(
            .name("swift"),
            arguments: Subprocess.Arguments(["package", "init", "--type", "executable"]),
            workingDirectory: .init(projectPath),
            output: .string(limit: .max),
            error: .string(limit: .max)
        )

        if !result.terminationStatus.isSuccess {
            let exitCode: Int
            switch result.terminationStatus {
            case .exited(let code), .unhandledException(let code):
                exitCode = Int(code)
            }
            throw CLIError.commandFailed(
                command: "swift package init --type executable",
                exitCode: Int32(exitCode),
                output: result.standardError ?? ""
            )
        }
    }

    private func initializePythonProject() async throws {
        // Create app.py
        let appPyPath =
            projectPath.hasSuffix("/") ? "\(projectPath)app.py" : "\(projectPath)/app.py"
        let appPyContent = #"""
            #!/usr/bin/env python3
            """
            Simple Hello World Python HTTP Server
            """

            from http.server import HTTPServer, BaseHTTPRequestHandler
            import json
            import os

            class HelloWorldHandler(BaseHTTPRequestHandler):
                def do_GET(self):
                    if self.path == '/':
                        self.send_response(200)
                        self.send_header('Content-type', 'text/html')
                        self.end_headers()
                        
                        html_content = """
                        <!DOCTYPE html charset="utf-8">
                        <html lang="en">
                        <head>
                            <title>Hello World Server</title>
                            <style>
                                body { 
                                    font-family: Arial, sans-serif; 
                                    max-width: 800px; 
                                    margin: 50px auto; 
                                    padding: 20px;
                                    background-color: #f5f5f5;
                                }
                                .container {
                                    background-color: white;
                                    padding: 30px;
                                    border-radius: 10px;
                                    box-shadow: 0 2px 10px rgba(0,0,0,0.1);
                                    text-align: center;
                                }
                                h1 { color: #333; }
                                .status { color: #28a745; font-weight: bold; }
                            </style>
                        </head>
                        <body>
                            <div class="container">
                                <h1>Hello World!</h1>
                                <p class="status">Server is running successfully!</p>
                                <p>This is a simple Python HTTP server running in a Docker container.</p>
                            </div>
                        </body>
                        </html>
                        """
                        self.wfile.write(html_content.encode())
                        
                    elif self.path == '/api/hello':
                        self.send_response(200)
                        self.send_header('Content-type', 'application/json')
                        self.end_headers()
                        
                        response = {
                            "message": "Hello World!",
                            "status": "success",
                            "server": "Python HTTP Server"
                        }
                        self.wfile.write(json.dumps(response, indent=2).encode())
                        
                    elif self.path == '/health':
                        self.send_response(200)
                        self.send_header('Content-type', 'application/json')
                        self.end_headers()
                        
                        health_response = {
                            "status": "healthy",
                            "message": "Server is running"
                        }
                        self.wfile.write(json.dumps(health_response).encode())
                        
                    else:
                        self.send_response(404)
                        self.send_header('Content-type', 'application/json')
                        self.end_headers()
                        
                        error_response = {
                            "error": "Not Found",
                            "message": f"Path {self.path} not found"
                        }
                        self.wfile.write(json.dumps(error_response).encode())

                def log_message(self, format, *args):
                    print(f"[{self.date_time_string()}] {format % args}", flush=True)

            def run_server(port=8000):
                server_address = ('0.0.0.0', port)
                httpd = HTTPServer(server_address, HelloWorldHandler)
                print(f"Starting server on {server_address[0]}:{server_address[1]}", flush=True)
                print(f"Visit http://localhost:{port} to see the Hello World page", flush=True)
                print(f"API endpoints available:", flush=True)
                print(f"  GET  /api/hello - JSON hello message", flush=True)
                print(f"  GET  /health - Health check", flush=True)
                print("Server is ready to accept connections...", flush=True)
                
                try:
                    httpd.serve_forever()
                except KeyboardInterrupt:
                    print("\nShutting down server...", flush=True)
                    httpd.shutdown()

            if __name__ == '__main__':
                port = int(os.environ.get('PORT', 8000))
                run_server(port)
            """#

        do {
            try appPyContent.write(
                to: URL(fileURLWithPath: appPyPath),
                atomically: true,
                encoding: .utf8
            )
        } catch {
            throw CLIError.fileCreationFailed(path: appPyPath, reason: error.localizedDescription)
        }

        // Create requirements.txt
        let requirementsPath =
            projectPath.hasSuffix("/")
            ? "\(projectPath)requirements.txt" : "\(projectPath)/requirements.txt"
        let requirementsContent = "debugpy\n"
        do {
            try requirementsContent.write(
                to: URL(fileURLWithPath: requirementsPath),
                atomically: true,
                encoding: .utf8
            )
        } catch {
            throw CLIError.fileCreationFailed(
                path: requirementsPath,
                reason: error.localizedDescription
            )
        }

        // Create Dockerfile
        let dockerfilePath =
            projectPath.hasSuffix("/") ? "\(projectPath)Dockerfile" : "\(projectPath)/Dockerfile"
        let dockerfileContent = #"""
            # Use Python 3.11 slim image as base
            FROM python:3.11-slim

            # Set working directory
            WORKDIR /app

            # Copy requirements first for better caching
            COPY requirements.txt .

            # Install dependencies
            RUN pip install --no-cache-dir -r requirements.txt

            # Copy application code
            COPY app.py .
            COPY entrypoint.sh .

            # Create a non-root user for security
            RUN useradd --create-home --shell /bin/bash app && \
                chmod +x entrypoint.sh && \
                chown -R app:app /app
            USER app

            # Expose port 8000 for the HTTP server
            EXPOSE 8000

            # Expose debugpy port 5678 for remote debugging
            EXPOSE 5678

            # Health check
            HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
                CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:8000/health')"

            # Run the application
            # Set DEBUG=true to enable debugpy, DEBUG_WAIT=true to wait for debugger
            CMD ["./entrypoint.sh"]
            """#
        do {
            try dockerfileContent.write(
                to: URL(fileURLWithPath: dockerfilePath),
                atomically: true,
                encoding: .utf8
            )
        } catch {
            throw CLIError.fileCreationFailed(
                path: dockerfilePath,
                reason: error.localizedDescription
            )
        }

        // Create entrypoint.sh
        let entrypointPath =
            projectPath.hasSuffix("/")
            ? "\(projectPath)entrypoint.sh" : "\(projectPath)/entrypoint.sh"
        let entrypointContent = """
            #!/bin/bash
            # Entrypoint script for running the app with optional debugpy
            exec python -m debugpy --listen "0.0.0.0:5678" app.py
            """
        do {
            try entrypointContent.write(
                to: URL(fileURLWithPath: entrypointPath),
                atomically: true,
                encoding: .utf8
            )
            _ = try await Subprocess.run(
                .name("chmod"),
                arguments: ["+x", entrypointPath],
                output: .discarded,
                error: .discarded
            )
        } catch {
            throw CLIError.fileCreationFailed(
                path: entrypointPath,
                reason: error.localizedDescription
            )
        }
    }

    private func createDefaultWendyJson(
        in projectPath: String,
        language: ProjectLanguage
    ) async throws {
        let fileManager = FileManager.default
        let wendyJsonPath =
            projectPath.hasSuffix("/") ? "\(projectPath)wendy.json" : "\(projectPath)/wendy.json"

        // Don't overwrite existing wendy.json
        if fileManager.fileExists(atPath: wendyJsonPath) {
            logger.debug("wendy.json already exists, skipping creation")
            return
        }

        // Get project name from directory
        let projectName = URL(fileURLWithPath: projectPath).lastPathComponent
        let appId =
            "com.example.\(projectName.lowercased().replacingOccurrences(of: "-", with: ""))"

        // Create default AppConfig
        var defaultConfig = AppConfig(
            appId: appId,
            version: "0.0.1",
            language: language.rawValue,
            entitlements: [
                .audio,
                .bluetooth(.init(mode: .bluez)),
                .network(.init(mode: .host)),
                .video(.init(mode: .all)),
                .persist(.init(name: "app-\(appId)", path: "/mnt/app")),
                .persist(.init(name: "wendy-shared", path: "/mnt/shared")),
            ]
        )

        if language == .python {
            defaultConfig.python = .init(sourceRoot: "/app")
            defaultConfig.entitlements.append(.network(.init(mode: .host)))
            // Shared cache for Hugging Face models (transformers, datasets, etc.)
            // Using a well-known name allows multiple apps to share downloaded models
            defaultConfig.entitlements.append(
                .persist(.init(name: "huggingface-cache", path: "/app/.cache/huggingface"))
            )
        }

        do {
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys]

            try encoder.encode(defaultConfig).write(to: URL(fileURLWithPath: wendyJsonPath))
            logger.debug(
                "Created wendy.json configuration file",
                metadata: [
                    "appId": .string(appId), "version": .string("0.0.1"),
                    "language": .string(language.rawValue),
                ]
            )
        } catch {
            throw CLIError.fileCreationFailed(
                path: wendyJsonPath,
                reason: error.localizedDescription
            )
        }
    }
}

enum ProjectLanguage: String, ExpressibleByArgument, CaseIterable {
    case swift
    case python
}
