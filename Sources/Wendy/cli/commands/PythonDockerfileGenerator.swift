import Foundation
import Noora

/// Generates Dockerfiles for Python projects that have requirements.txt but no Dockerfile
struct PythonDockerfileGenerator {

    /// Detected Python framework from requirements.txt
    enum Framework: String, CaseIterable {
        case flask
        case fastapi
        case django
        case gunicorn
        case uvicorn
        case none

        var displayName: String {
            switch self {
            case .flask: return "Flask"
            case .fastapi: return "FastAPI"
            case .django: return "Django"
            case .gunicorn: return "Gunicorn"
            case .uvicorn: return "Uvicorn"
            case .none: return "None"
            }
        }
    }

    /// System packages required by common Python packages
    private static let systemDependencies: [String: [String]] = [
        "psycopg2": ["libpq-dev"],
        "psycopg": ["libpq-dev"],
        "psycopg2-binary": [],  // Binary wheel, no system deps needed
        "pillow": ["libjpeg-dev", "zlib1g-dev", "libpng-dev"],
        "pil": ["libjpeg-dev", "zlib1g-dev", "libpng-dev"],
        "mysqlclient": ["default-libmysqlclient-dev", "pkg-config"],
        "lxml": ["libxml2-dev", "libxslt-dev"],
        "cryptography": ["libffi-dev", "libssl-dev"],
        "cffi": ["libffi-dev"],
        "greenlet": ["gcc"],
        "grpcio": ["gcc"],
        "opencv-python": ["libgl1", "libglib2.0-0"],
        "opencv-python-headless": ["libgl1", "libglib2.0-0"],
    ]

    /// Packages that require specialized installation (e.g., GPU-optimized builds, platform-specific wheels)
    /// These may not work correctly with a generic pip install in a standard Docker image
    struct SpecializedPackage {
        let name: String
        let importNames: [String]
        let warning: String
    }

    private static let specializedPackages: [SpecializedPackage] = [
        SpecializedPackage(
            name: "PyTorch",
            importNames: ["torch", "torchvision", "torchaudio"],
            warning:
                "PyTorch requires platform-specific installation. For NVIDIA Jetson, use wheels from pypi.jetson-ai-lab.io"
        ),
        SpecializedPackage(
            name: "TensorFlow",
            importNames: ["tensorflow", "tf"],
            warning:
                "TensorFlow requires platform-specific installation. For NVIDIA Jetson, use NVIDIA's optimized builds"
        ),
        SpecializedPackage(
            name: "JAX",
            importNames: ["jax", "jaxlib"],
            warning: "JAX requires platform-specific installation for GPU support"
        ),
        SpecializedPackage(
            name: "ONNX Runtime",
            importNames: ["onnxruntime"],
            warning: "ONNX Runtime requires platform-specific builds for GPU acceleration"
        ),
        SpecializedPackage(
            name: "CuPy",
            importNames: ["cupy"],
            warning: "CuPy requires CUDA-specific installation"
        ),
        SpecializedPackage(
            name: "PyCUDA",
            importNames: ["pycuda"],
            warning: "PyCUDA requires CUDA toolkit and platform-specific installation"
        ),
    ]

    /// Common entry point filenames in priority order
    private static let commonEntryPoints = [
        "main.py",
        "app.py",
        "__main__.py",
        "manage.py",  // Django
        "wsgi.py",
        "asgi.py",
        "server.py",
        "run.py",
    ]

    let projectPath: String
    let isJetsonDevice: Bool

    init(
        projectPath: String = FileManager.default.currentDirectoryPath,
        isJetsonDevice: Bool = true  // TODO: Detect actual device type
    ) {
        self.projectPath = projectPath
        self.isJetsonDevice = isJetsonDevice
    }

    // MARK: - Python Version Detection

    /// Detects Python version from project files, returns nil if not found
    func detectPythonVersion() -> String? {
        // 1. Check .python-version file
        if let version = readPythonVersionFile() {
            return normalizeVersion(version)
        }

        // 2. Check pyproject.toml
        if let version = readPyprojectTomlVersion() {
            return normalizeVersion(version)
        }

        // 3. Check runtime.txt (Heroku-style)
        if let version = readRuntimeTxt() {
            return normalizeVersion(version)
        }

        return nil
    }

    /// Returns the Python version to use (detected or default)
    func getPythonVersion() -> String {
        detectPythonVersion() ?? "3.12"
    }

    private func readPythonVersionFile() -> String? {
        let path = "\(projectPath)/.python-version"
        guard let content = try? String(contentsOfFile: path, encoding: .utf8) else {
            return nil
        }
        return content.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func readPyprojectTomlVersion() -> String? {
        let path = "\(projectPath)/pyproject.toml"
        guard let content = try? String(contentsOfFile: path, encoding: .utf8) else {
            return nil
        }

        // Look for requires-python = ">=3.11" or similar
        // Simple regex-free parsing
        for line in content.components(separatedBy: .newlines) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("requires-python") {
                // Extract version from something like: requires-python = ">=3.11"
                if let quoteStart = trimmed.firstIndex(of: "\""),
                    let quoteEnd = trimmed.lastIndex(of: "\""),
                    quoteStart < quoteEnd
                {
                    let versionSpec = String(trimmed[trimmed.index(after: quoteStart)..<quoteEnd])
                    // Extract just the version number
                    return extractVersionNumber(from: versionSpec)
                }
            }
        }

        return nil
    }

    private func readRuntimeTxt() -> String? {
        let path = "\(projectPath)/runtime.txt"
        guard let content = try? String(contentsOfFile: path, encoding: .utf8) else {
            return nil
        }
        // Format: python-3.11.4
        let trimmed = content.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.hasPrefix("python-") {
            return String(trimmed.dropFirst("python-".count))
        }
        return nil
    }

    /// Extracts version number from version specifier like ">=3.11", "~=3.10", "==3.12.1"
    private func extractVersionNumber(from spec: String) -> String? {
        var version = spec
        // Remove common prefixes
        for prefix in [">=", "<=", "~=", "==", ">", "<", "^"] {
            if version.hasPrefix(prefix) {
                version = String(version.dropFirst(prefix.count))
                break
            }
        }
        // Remove any trailing specifiers (e.g., ",<4.0")
        if let commaIndex = version.firstIndex(of: ",") {
            version = String(version[..<commaIndex])
        }
        return version.trimmingCharacters(in: .whitespaces)
    }

    /// Normalizes version to major.minor format for Docker image tag
    private func normalizeVersion(_ version: String) -> String {
        let parts = version.components(separatedBy: ".")
        if parts.count >= 2 {
            return "\(parts[0]).\(parts[1])"
        }
        return version
    }

    // MARK: - Specialized Package Detection

    /// Detects packages that require specialized installation by scanning Python files for imports
    func detectSpecializedPackages() -> [SpecializedPackage] {
        let fileManager = FileManager.default
        guard let contents = try? fileManager.contentsOfDirectory(atPath: projectPath) else {
            return []
        }

        // Collect all imports from Python files
        var allImports = Set<String>()

        for file in contents where file.hasSuffix(".py") {
            let filePath = "\(projectPath)/\(file)"
            guard let content = try? String(contentsOfFile: filePath, encoding: .utf8) else {
                continue
            }

            // Parse imports from the file
            for line in content.components(separatedBy: .newlines) {
                let trimmed = line.trimmingCharacters(in: .whitespaces)

                // Match "import X" or "from X import ..."
                if trimmed.hasPrefix("import ") {
                    let rest = trimmed.dropFirst("import ".count)
                    // Handle "import X, Y, Z" and "import X as Y"
                    let modules = rest.components(separatedBy: ",")
                    for module in modules {
                        let moduleName =
                            module
                            .trimmingCharacters(in: .whitespaces)
                            .components(separatedBy: " ")[0]  // Remove "as ..." suffix
                            .components(separatedBy: ".")[0]  // Get top-level module
                        allImports.insert(moduleName.lowercased())
                    }
                } else if trimmed.hasPrefix("from ") {
                    // "from X import ..." or "from X.Y import ..."
                    let rest = trimmed.dropFirst("from ".count)
                    if let importIndex = rest.range(of: " import ") {
                        let modulePath = String(rest[..<importIndex.lowerBound])
                        let topModule = modulePath.components(separatedBy: ".")[0]
                        allImports.insert(topModule.lowercased())
                    }
                }
            }
        }

        // Also check requirements.txt if it exists
        for package in parseRequirements() {
            allImports.insert(package.lowercased())
        }

        // Find which specialized packages are used
        var detectedPackages: [SpecializedPackage] = []
        for specializedPkg in Self.specializedPackages {
            for importName in specializedPkg.importNames {
                if allImports.contains(importName.lowercased()) {
                    detectedPackages.append(specializedPkg)
                    break
                }
            }
        }

        return detectedPackages
    }

    // MARK: - Entry Point Detection

    /// Detects the entry point file for the Python application
    func detectEntryPoint() -> String? {
        let fileManager = FileManager.default

        // Check common entry points in priority order
        for entryPoint in Self.commonEntryPoints {
            let path = "\(projectPath)/\(entryPoint)"
            if fileManager.fileExists(atPath: path) {
                return entryPoint
            }
        }

        return nil
    }

    /// Returns all Python files in the project root (for user selection)
    func listPythonFiles() -> [String] {
        let fileManager = FileManager.default
        guard let contents = try? fileManager.contentsOfDirectory(atPath: projectPath) else {
            return []
        }

        return
            contents
            .filter { $0.hasSuffix(".py") && !$0.hasPrefix("_") && $0 != "__init__.py" }
            .sorted()
    }

    /// Prompts user to select an entry point if auto-detection fails
    func promptForEntryPoint(autoAccept: Bool) -> String? {
        let pythonFiles = listPythonFiles()

        if pythonFiles.isEmpty {
            return nil
        }

        if pythonFiles.count == 1 {
            return pythonFiles[0]
        }

        if autoAccept {
            // In auto-accept mode, use first available file
            return pythonFiles.first
        }

        // Let user choose
        return Noora().singleChoicePrompt(
            title: "Select entry point",
            question: "Which Python file is the main entry point?",
            options: pythonFiles
        )
    }

    // MARK: - Framework Detection

    /// Detects the web framework from requirements.txt
    func detectFramework() -> Framework {
        guard let requirements = readRequirements() else {
            return .none
        }

        let lowercased = requirements.lowercased()

        // Check in order of specificity
        if lowercased.contains("fastapi") {
            return .fastapi
        }
        if lowercased.contains("django") {
            return .django
        }
        if lowercased.contains("flask") {
            return .flask
        }
        if lowercased.contains("uvicorn") {
            return .uvicorn
        }
        if lowercased.contains("gunicorn") {
            return .gunicorn
        }

        return .none
    }

    /// Parses requirements.txt and returns package names
    func parseRequirements() -> [String] {
        guard let content = readRequirements() else {
            return []
        }

        var packages: [String] = []

        for line in content.components(separatedBy: .newlines) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)

            // Skip comments and empty lines
            if trimmed.isEmpty || trimmed.hasPrefix("#") {
                continue
            }

            // Skip -r, -e, and other flags
            if trimmed.hasPrefix("-") {
                continue
            }

            // Extract package name (before any version specifier)
            var packageName = trimmed
            for separator in [">=", "<=", "~=", "==", ">", "<", "[", ";"] {
                if let index = packageName.range(of: separator) {
                    packageName = String(packageName[..<index.lowerBound])
                }
            }

            packages.append(packageName.lowercased().trimmingCharacters(in: .whitespaces))
        }

        return packages
    }

    private func readRequirements() -> String? {
        let path = "\(projectPath)/requirements.txt"
        return try? String(contentsOfFile: path, encoding: .utf8)
    }

    // MARK: - System Dependencies

    /// Detects required system packages based on Python dependencies
    func detectSystemDependencies() -> [String] {
        let packages = parseRequirements()
        var systemDeps = Set<String>()

        for package in packages {
            if let deps = Self.systemDependencies[package] {
                for dep in deps {
                    systemDeps.insert(dep)
                }
            }
        }

        return systemDeps.sorted()
    }

    // MARK: - Dockerfile Generation

    /// Generates the CMD instruction based on detected framework and entry point
    func generateCmd(framework: Framework, entryPoint: String) -> String {
        switch framework {
        case .fastapi:
            // Detect the app variable name (commonly "app" in main.py or app.py)
            let module = entryPoint.replacingOccurrences(of: ".py", with: "")
            return #"CMD ["uvicorn", "\#(module):app", "--host", "0.0.0.0", "--port", "8000"]"#

        case .django:
            return #"CMD ["python", "manage.py", "runserver", "0.0.0.0:8000"]"#

        case .flask:
            let module = entryPoint.replacingOccurrences(of: ".py", with: "")
            return
                #"CMD ["flask", "--app", "\#(module)", "run", "--host", "0.0.0.0", "--port", "8000"]"#

        case .gunicorn:
            let module = entryPoint.replacingOccurrences(of: ".py", with: "")
            return #"CMD ["gunicorn", "--bind", "0.0.0.0:8000", "\#(module):app"]"#

        case .uvicorn:
            let module = entryPoint.replacingOccurrences(of: ".py", with: "")
            return #"CMD ["uvicorn", "\#(module):app", "--host", "0.0.0.0", "--port", "8000"]"#

        case .none:
            return #"CMD ["python", "\#(entryPoint)"]"#
        }
    }

    /// Checks if PyTorch is used in the project
    func usesPyTorch() -> Bool {
        let specializedPackages = detectSpecializedPackages()
        return specializedPackages.contains { $0.name == "PyTorch" }
    }

    /// Checks if TensorFlow is used in the project
    func usesTensorFlow() -> Bool {
        let specializedPackages = detectSpecializedPackages()
        return specializedPackages.contains { $0.name == "TensorFlow" }
    }

    /// Gets additional Python packages from requirements.txt, excluding PyTorch-related packages
    private func getAdditionalPackages(excluding: Set<String>) -> [String] {
        let packages = parseRequirements()
        return packages.filter { pkg in
            !excluding.contains(pkg.lowercased())
        }
    }

    /// Generates a complete Dockerfile for the Python project
    func generateDockerfile(entryPoint: String) -> String {
        let specializedPackages = detectSpecializedPackages()
        let needsGPUOptimization = specializedPackages.contains {
            $0.name == "PyTorch" || $0.name == "TensorFlow"
        }

        // Use Jetson-optimized Dockerfile if targeting Jetson and using GPU packages
        if isJetsonDevice && needsGPUOptimization {
            return generateJetsonDockerfile(entryPoint: entryPoint)
        } else {
            return generateStandardDockerfile(entryPoint: entryPoint)
        }
    }

    /// Generates a Jetson-optimized Dockerfile for GPU/ML workloads
    private func generateJetsonDockerfile(entryPoint: String) -> String {
        let framework = detectFramework()
        let cmd = generateCmd(framework: framework, entryPoint: entryPoint)

        // Packages that should be installed from Jetson's PyPI, not standard PyPI
        let jetsonPackages: Set<String> = [
            "torch", "torchvision", "torchaudio", "tensorflow",
            "numpy",  // We pin numpy separately
        ]

        // Get additional packages excluding Jetson-specific ones
        let additionalPackages = getAdditionalPackages(excluding: jetsonPackages)

        // Detect common ML packages from imports
        let mlPackages = detectCommonMLPackages()

        var dockerfile = """
            # Generated by wendy CLI - Optimized for NVIDIA Jetson
            FROM ubuntu:22.04

            # Workaround for containerd overlayfs snapshot issues
            RUN mkdir -p /usr/local/bin

            # Install system dependencies including OpenBLAS (required by PyTorch)
            RUN apt-get update && apt-get install -y --no-install-recommends \\
                python3-pip \\
                python3-dev \\
                build-essential \\
                git \\
                libopenblas-dev \\
                libblas-dev \\
                liblapack-dev \\
                libgomp1 \\
                && rm -rf /var/lib/apt/lists/*

            # Install PyTorch from NVIDIA's Jetson-optimized wheels
            # Pre-compiled for ARM64 and optimized for Jetson with CUDA 12.6
            # This leverages CDI-mapped CUDA/cuDNN from the host system
            RUN pip3 install --no-cache-dir \\
                torch==2.8.0 \\
                --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

            # Pin NumPy to 1.x (Jetson PyTorch was compiled against NumPy 1.x)
            RUN pip3 install --no-cache-dir numpy==1.26.4

            """

        // Combine ML packages and additional packages, removing duplicates
        var allPackages = Set(mlPackages)
        for pkg in additionalPackages {
            allPackages.insert(pkg)
        }

        // Install additional packages
        if !allPackages.isEmpty {
            dockerfile += """
                # Install additional Python dependencies
                RUN pip3 install --no-cache-dir \\
                    \(allPackages.sorted().joined(separator: " \\\n    "))

                """
        }

        // Verify PyTorch version
        dockerfile += """
            # Verify we got the correct PyTorch version (will fail build if wrong)
            # Use pip show instead of import to avoid CUDA library loading during build
            RUN pip3 show torch | grep "Version: 2.8.0" && echo "Correct PyTorch version installed"

            # Set working directory and copy application code
            WORKDIR /app
            COPY . .

            # Set environment variables
            ENV PYTHONUNBUFFERED=1
            ENV HF_HOME=/app/.cache/huggingface
            ENV TRANSFORMERS_CACHE=/app/.cache/huggingface

            # Run the application
            \(cmd.replacingOccurrences(of: "python", with: "python3"))
            """

        return dockerfile
    }

    /// Detects common ML packages that should be installed for PyTorch projects
    private func detectCommonMLPackages() -> [String] {
        var packages: [String] = []
        let allImports = getAllImports()

        // Check for transformers (HuggingFace)
        if allImports.contains("transformers") {
            packages.append("transformers")
            packages.append("accelerate")
            packages.append("sentencepiece")
        }

        // Check for Flask (common for ML APIs)
        if allImports.contains("flask") {
            packages.append("flask")
        }

        return packages
    }

    /// Gets all imports from Python files in the project
    private func getAllImports() -> Set<String> {
        let fileManager = FileManager.default
        guard let contents = try? fileManager.contentsOfDirectory(atPath: projectPath) else {
            return []
        }

        var allImports = Set<String>()

        for file in contents where file.hasSuffix(".py") {
            let filePath = "\(projectPath)/\(file)"
            guard let content = try? String(contentsOfFile: filePath, encoding: .utf8) else {
                continue
            }

            for line in content.components(separatedBy: .newlines) {
                let trimmed = line.trimmingCharacters(in: .whitespaces)

                if trimmed.hasPrefix("import ") {
                    let rest = trimmed.dropFirst("import ".count)
                    let modules = rest.components(separatedBy: ",")
                    for module in modules {
                        let moduleName =
                            module
                            .trimmingCharacters(in: .whitespaces)
                            .components(separatedBy: " ")[0]
                            .components(separatedBy: ".")[0]
                        allImports.insert(moduleName.lowercased())
                    }
                } else if trimmed.hasPrefix("from ") {
                    let rest = trimmed.dropFirst("from ".count)
                    if let importIndex = rest.range(of: " import ") {
                        let modulePath = String(rest[..<importIndex.lowerBound])
                        let topModule = modulePath.components(separatedBy: ".")[0]
                        allImports.insert(topModule.lowercased())
                    }
                }
            }
        }

        return allImports
    }

    /// Generates a standard Dockerfile for simple Python projects
    private func generateStandardDockerfile(entryPoint: String) -> String {
        let pythonVersion = getPythonVersion()
        let framework = detectFramework()
        let systemDeps = detectSystemDependencies()
        let cmd = generateCmd(framework: framework, entryPoint: entryPoint)
        let hasRequirements = FileManager.default.fileExists(
            atPath: "\(projectPath)/requirements.txt"
        )
        let hasPyproject = FileManager.default.fileExists(atPath: "\(projectPath)/pyproject.toml")

        var dockerfile = """
            # Generated by wendy CLI
            FROM python:\(pythonVersion)

            WORKDIR /app

            """

        // Add system dependencies if needed
        if !systemDeps.isEmpty {
            dockerfile += """
                # Install system dependencies required by Python packages
                RUN apt-get update && apt-get install -y --no-install-recommends \\
                    \(systemDeps.joined(separator: " \\\n    ")) \\
                    && rm -rf /var/lib/apt/lists/*

                """
        }

        // Install Python dependencies based on what files are available
        if hasRequirements {
            dockerfile += """
                # Install Python dependencies
                COPY requirements.txt .
                RUN pip install --no-cache-dir -r requirements.txt

                """
        } else if hasPyproject {
            dockerfile += """
                # Install Python dependencies from pyproject.toml
                COPY pyproject.toml .
                RUN pip install --no-cache-dir .

                """
        }

        dockerfile += """
            # Copy application code
            COPY . .

            # Expose the default port
            EXPOSE 8000

            # Run the application
            \(cmd)
            """

        return dockerfile
    }

    /// Writes the generated Dockerfile to the project directory
    func writeDockerfile(entryPoint: String) throws {
        let content = generateDockerfile(entryPoint: entryPoint)
        let path = "\(projectPath)/Dockerfile"
        try content.write(toFile: path, atomically: true, encoding: .utf8)
    }
}
